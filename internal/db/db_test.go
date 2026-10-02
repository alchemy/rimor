package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// walk renders the whole catalog tree as indented "name detail" lines,
// using a Pool so databases are reached the way the explorer reaches them.
// With sample set, only the first table and view of each folder is opened,
// which keeps walks of large demo databases short.
func walk(t *testing.T, cfg Config, sample bool) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := NewPool(cfg)
	opened := map[string]bool{}
	defer pool.Close()

	var out []string
	var visit func(parent *Object, depth int)
	visit = func(parent *Object, depth int) {
		objs, err := pool.Children(ctx, parent)
		if err != nil {
			t.Fatalf("children of %+v: %v", parent, err)
		}
		for _, o := range objs {
			line := strings.Repeat("  ", depth) + o.Name
			if o.Detail != "" {
				line += " " + o.Detail
			}
			if o.Primary {
				line += " *"
			}
			out = append(out, line)
			if sample && (o.Kind == KindTable || o.Kind == KindView) {
				key := fmt.Sprint(o.Database, o.Schema, o.Kind)
				if opened[key] {
					continue
				}
				opened[key] = true
			}
			if o.Expandable() {
				visit(&o, depth+1)
			}
		}
	}
	visit(nil, 0)
	t.Log("\n" + strings.Join(out, "\n"))
	return out
}

func requireLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing line %q", w)
		}
	}
}

func TestSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE customer (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT UNIQUE)`,
		`CREATE TABLE "order line" (id INTEGER, customer_id INTEGER REFERENCES customer, PRIMARY KEY (id, customer_id))`,
		`CREATE INDEX order_customer ON "order line"(customer_id)`,
		`CREATE VIEW names AS SELECT name FROM customer`,
		`CREATE TRIGGER touch AFTER INSERT ON customer BEGIN SELECT 1; END`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()

	got := walk(t, Config{Driver: SQLite, DSN: path}, false)
	requireLines(t, got,
		"Tables",
		"  customer",
		"      id INTEGER *",
		"      name TEXT",
		"  order line",
		"      customer_id INTEGER *",
		"      order_customer",
		"      sqlite_autoindex_order line_1 primary *",
		"      sqlite_autoindex_customer_1 unique",
		"  names",
		"  order_customer order line",
		"  touch customer",
	)
}

func TestSQLiteMissingFile(t *testing.T) {
	_, err := Open(context.Background(), Config{Driver: SQLite, DSN: filepath.Join(t.TempDir(), "nope.db")})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// Server-backed tests run when a DSN is provided, e.g.
//
//	RIMOR_TEST_POSTGRES=postgres://postgres@localhost:5432/postgres
//	RIMOR_TEST_SQLSERVER=sqlserver://sa:pass@localhost:1433?database=master
func serverDSN(t *testing.T, env string) string {
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skip(env + " not set")
	}
	return dsn
}

func TestPostgres(t *testing.T) {
	walk(t, Config{Driver: Postgres, DSN: serverDSN(t, "RIMOR_TEST_POSTGRES")}, false)
}

func TestSQLServer(t *testing.T) {
	walk(t, Config{Driver: SQLServer, DSN: serverDSN(t, "RIMOR_TEST_SQLSERVER")}, true)
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "nested", "connections.json")}
	if got, err := s.Load(); err != nil || got != nil {
		t.Fatalf("empty load: %v %v", got, err)
	}
	want := []Config{{Name: "local", Driver: Postgres, DSN: "postgres://x"}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Errorf("perm = %v, want 0600", perm)
	}
	got, err := s.Load()
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("load = %v %v", got, err)
	}
}

func TestMSSQLWithDatabase(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"sqlserver://u:p%40x@host?database=old&encrypt=disable", "sqlserver://u:p%40x@host?database=erp&encrypt=disable"},
		{"sqlserver://u:p@host:1433", "sqlserver://u:p@host:1433?database=erp"},
		{"server=host;user id=u;Initial Catalog=old;", "server=host;user id=u;database=erp"},
	} {
		got, err := mssqlWithDatabase(c.in, "erp")
		if err != nil || got != c.want {
			t.Errorf("mssqlWithDatabase(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestMSSQLType(t *testing.T) {
	for _, c := range []struct {
		name             string
		length, prec, sc int
		want             string
	}{
		{"nvarchar", 100, 0, 0, "nvarchar(50)"},
		{"nvarchar", -1, 0, 0, "nvarchar(max)"},
		{"varchar", 20, 0, 0, "varchar(20)"},
		{"decimal", 9, 10, 2, "decimal(10,2)"},
		{"int", 4, 10, 0, "int"},
	} {
		if got := mssqlType(c.name, c.length, c.prec, c.sc); got != c.want {
			t.Errorf("mssqlType(%s) = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestStoreInit(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "config", "rimor", "connections.json")}

	// First run: the folder and an empty connections file appear.
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(filepath.Dir(s.Path))
	if err != nil || !dir.IsDir() {
		t.Fatalf("dir: %v %v", dir, err)
	}
	if runtime.GOOS != "windows" && dir.Mode().Perm() != 0o700 {
		t.Errorf("dir perm = %v", dir.Mode().Perm()) // Windows has no Unix permissions
	}
	data, err := os.ReadFile(s.Path)
	if err != nil || strings.TrimSpace(string(data)) != "{\n  \"connections\": []\n}" {
		t.Fatalf("file = %q, %v", data, err)
	}

	// Later runs keep what is there.
	want := []Config{{Name: "local", Driver: SQLite, DSN: "x.db"}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(); err != nil || !slices.Equal(got, want) {
		t.Fatalf("after second Init = %v, %v", got, err)
	}
}
