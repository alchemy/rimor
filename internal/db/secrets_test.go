package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// driverPassword is the password a driver would use for a connection
// string, as its own parser reads it.
func driverPassword(t *testing.T, d Driver, dsn string) string {
	t.Helper()
	switch d {
	case Postgres:
		c, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("pgx cannot parse %q: %v", dsn, err)
		}
		return c.Password
	case SQLServer:
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("go-mssqldb cannot parse %q: %v", dsn, err)
		}
		return c.Password
	}
	return ""
}

func TestSplitAndJoinPassword(t *testing.T) {
	// The drivers fall back on these, and CI images set some (Windows:
	// PGPASSWORD=root); only the connection string may count here.
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("PGSERVICEFILE", filepath.Join(t.TempDir(), "none"))
	for _, c := range []struct {
		driver          Driver
		dsn, clean, pwd string
	}{
		{Postgres, "postgres://alice:s3cr%40t@db.local:5432/erp?sslmode=disable", "postgres://alice@db.local:5432/erp?sslmode=disable", "s3cr@t"},
		{Postgres, "postgresql://alice:pw@/erp?host=/run/postgresql", "postgresql://alice@/erp?host=/run/postgresql", "pw"},
		{Postgres, "host=db.local user=alice password=plain dbname=erp", "host=db.local user=alice dbname=erp", "plain"},
		{Postgres, `host=db.local password='it\'s a \\ secret' dbname='my  db'`, "host=db.local dbname='my  db'", `it's a \ secret`},
		{Postgres, "host=db.local user=alice password=last", "host=db.local user=alice", "last"},
		{SQLServer, "sqlserver://sa:p%3Bss@sql.local?database=erp&TrustServerCertificate=true", "sqlserver://sa@sql.local?database=erp&TrustServerCertificate=true", "p;ss"},
		{SQLServer, `server=sql.local;user id=sa;password="a;b}c";database=erp`, "server=sql.local;user id=sa;database=erp", "a;b}c"},
		{SQLServer, "server=sql.local;password=first;pwd=last;database=erp", "server=sql.local;database=erp", "last"},
		{SQLServer, "Server=sql.local;User Id=sa;Pwd=x9", "Server=sql.local;User Id=sa;", "x9"},
		{SQLServer, `server=sql.local;password="say ""hi""";database=erp`, "server=sql.local;database=erp", `say "hi"`},
	} {
		clean, pwd, ok := splitPassword(c.driver, c.dsn)
		if !ok || clean != c.clean || pwd != c.pwd {
			t.Errorf("split %q:\n got %q, %q, %v\nwant %q, %q", c.dsn, clean, pwd, ok, c.clean, c.pwd)
			continue
		}
		if got := driverPassword(t, c.driver, clean); got != "" {
			t.Errorf("%q still has a password for the driver: %q", clean, got)
		}
		joined := joinPassword(c.driver, clean, pwd)
		if got := driverPassword(t, c.driver, joined); got != c.pwd {
			t.Errorf("join %q: the driver reads %q, want %q", joined, got, c.pwd)
		}
		if got := driverPassword(t, c.driver, c.dsn); got != c.pwd {
			t.Errorf("the fixture is wrong: the driver reads %q from %q", got, c.dsn)
		}
	}

	// Nothing to take: no password, SQLite, a form this does not read.
	for _, c := range []struct {
		driver Driver
		dsn    string
	}{
		{Postgres, "postgres://alice@db.local/erp"},
		{Postgres, "host=db.local user=alice"},
		{Postgres, "host=db.local password='unterminated"},
		{SQLServer, "sqlserver://sa@sql.local?database=erp"},
		{SQLServer, "odbc:server=sql.local;pwd={x;y}"},
		{SQLite, "/home/me/password=x.db"},
	} {
		if clean, pwd, ok := splitPassword(c.driver, c.dsn); ok || clean != c.dsn || pwd != "" {
			t.Errorf("split %q: %q, %q, %v", c.dsn, clean, pwd, ok)
		}
	}
}

// fakeKeychain is an in-memory keychain; fail makes it refuse.
type fakeKeychain struct {
	entries map[string]string
	fail    error
}

func newFakeKeychain() *fakeKeychain { return &fakeKeychain{entries: map[string]string{}} }

func (k *fakeKeychain) Get(name string) (string, error) {
	if k.fail != nil {
		return "", k.fail
	}
	p, ok := k.entries[name]
	if !ok {
		return "", errors.New("not found")
	}
	return p, nil
}

func (k *fakeKeychain) Set(name, password string) error {
	if k.fail != nil {
		return k.fail
	}
	k.entries[name] = password
	return nil
}

func (k *fakeKeychain) Delete(name string) error {
	delete(k.entries, name)
	return nil
}

func fileText(t *testing.T, s Store) string {
	t.Helper()
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStoreKeepsPasswordsInTheKeychain(t *testing.T) {
	keys := newFakeKeychain()
	s := Store{Path: filepath.Join(t.TempDir(), "connections.json"), Secrets: keys}
	erp := Config{Name: "ERP", Driver: SQLServer, DSN: "sqlserver://sa:Hunter2@sql.local?database=erp"}
	shop := Config{Name: "shop", Driver: Postgres, DSN: "host=pg.local user=app password='Tr0ub4dor' dbname=shop"}
	lite := Config{Name: "lite", Driver: SQLite, DSN: "/tmp/x.db"}
	if err := s.Save([]Config{erp, shop, lite}); err != nil {
		t.Fatal(err)
	}

	text := fileText(t, s)
	for _, secret := range []string{"Hunter2", "Tr0ub4dor"} {
		if strings.Contains(text, secret) {
			t.Errorf("connections.json holds %q:\n%s", secret, text)
		}
	}
	if !strings.Contains(text, `"secret": "keychain"`) || keys.entries["ERP"] != "Hunter2" || keys.entries["shop"] != "Tr0ub4dor" {
		t.Errorf("file:\n%s\nkeychain %v", text, keys.entries)
	}
	if _, ok := keys.entries["lite"]; ok {
		t.Error("an SQLite connection got a keychain entry")
	}

	// Loading puts them back, where the drivers find them.
	got, err := s.Load()
	if err != nil || len(got) != 3 {
		t.Fatal(got, err)
	}
	if driverPassword(t, SQLServer, got[0].DSN) != "Hunter2" || driverPassword(t, Postgres, got[1].DSN) != "Tr0ub4dor" || got[2] != lite {
		t.Errorf("loaded %+v", got)
	}

	// Saving what was loaded changes nothing.
	if err := s.Save(got); err != nil || fileText(t, s) != text {
		t.Errorf("resave: %v\n%s", err, fileText(t, s))
	}

	// Renaming moves the entry; deleting removes it.
	got[0].Name = "ERP prod"
	if err := s.Save(got[:2]); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.entries["ERP"]; ok || keys.entries["ERP prod"] != "Hunter2" {
		t.Errorf("after rename %v", keys.entries)
	}
	if err := s.Save(got[1:2]); err != nil {
		t.Fatal(err)
	}
	if len(keys.entries) != 1 || keys.entries["shop"] == "" {
		t.Errorf("after delete %v", keys.entries)
	}

	// A password removed from the connection leaves the keychain too.
	got[1].DSN = "host=pg.local user=app dbname=shop"
	got[1].Secret = ""
	if err := s.Save(got[1:2]); err != nil || len(keys.entries) != 0 {
		t.Errorf("after removing the password: %v %v", err, keys.entries)
	}
}

func TestStoreWithoutAKeychain(t *testing.T) {
	keys := newFakeKeychain()
	keys.fail = errors.New("no secret service")
	s := Store{Path: filepath.Join(t.TempDir(), "connections.json"), Secrets: keys}
	erp := Config{Name: "ERP", Driver: SQLServer, DSN: "sqlserver://sa:Hunter2@sql.local?database=erp"}

	err := s.Save([]Config{erp})
	var ke *KeychainError
	if !errors.As(err, &ke) || !strings.Contains(err.Error(), "no secret service") {
		t.Fatalf("save: %v", err)
	}
	// Saved anyway, the old way.
	if text := fileText(t, s); !strings.Contains(text, "Hunter2") || strings.Contains(text, "keychain") {
		t.Errorf("file:\n%s", text)
	}
	if got, _ := s.Load(); len(got) != 1 || got[0].DSN != erp.DSN || got[0].SecretErr != nil {
		t.Errorf("loaded %+v", got)
	}
}

func TestStoreWhenTheKeychainCannotAnswer(t *testing.T) {
	keys := newFakeKeychain()
	s := Store{Path: filepath.Join(t.TempDir(), "connections.json"), Secrets: keys}
	erp := Config{Name: "ERP", Driver: SQLServer, DSN: "sqlserver://sa:Hunter2@sql.local?database=erp"}
	if err := s.Save([]Config{erp}); err != nil {
		t.Fatal(err)
	}

	keys.fail = errors.New("the keyring is locked")
	got, err := s.Load()
	if err != nil || got[0].SecretErr == nil || !strings.Contains(got[0].SecretErr.Error(), "locked") {
		t.Fatalf("load: %+v %v", got, err)
	}
	if _, err := Open(t.Context(), got[0]); err == nil || !strings.Contains(err.Error(), "keychain") {
		t.Errorf("connecting without the password: %v", err)
	}

	// Saving meanwhile keeps the mark, and the entry.
	keys.fail = nil
	if err := s.Save(got); err != nil {
		t.Fatal(err)
	}
	if keys.entries["ERP"] != "Hunter2" || !strings.Contains(fileText(t, s), `"secret": "keychain"`) {
		t.Errorf("the password was lost: %v\n%s", keys.entries, fileText(t, s))
	}
	if got, _ := s.Load(); driverPassword(t, SQLServer, got[0].DSN) != "Hunter2" {
		t.Errorf("reloaded %+v", got)
	}

	// A store without a keychain cannot read the mark either.
	plain := Store{Path: s.Path}
	if got, _ := plain.Load(); got[0].SecretErr == nil {
		t.Error("no error for a password it cannot reach")
	}
}

func TestMoveToKeychain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	old := Store{Path: path} // as rimor saved before keychain support
	erp := Config{Name: "ERP", Driver: SQLServer, DSN: "server=sql.local;user id=sa;password=Hunter2;database=erp"}
	if err := old.Save([]Config{erp}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fileText(t, old), "Hunter2") {
		t.Fatal("fixture")
	}

	keys := newFakeKeychain()
	s := Store{Path: path, Secrets: keys}
	if err := s.MoveToKeychain(); err != nil {
		t.Fatal(err)
	}
	if text := fileText(t, s); strings.Contains(text, "Hunter2") || keys.entries["ERP"] != "Hunter2" {
		t.Errorf("not moved: %v\n%s", keys.entries, text)
	}
	info, _ := os.Stat(path)
	if mode := info.Mode().Perm(); mode != 0o600 && os.PathSeparator == '/' {
		t.Errorf("file mode %v", mode)
	}

	// Nothing left to move: the file is not rewritten.
	before, _ := os.Stat(path)
	if err := s.MoveToKeychain(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("rewritten with nothing to move")
	}
}
