package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

func TestParseTarget(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.db")
	os.WriteFile(file, nil, 0o600)

	for _, c := range []struct {
		arg    string
		driver Driver
		name   string
	}{
		{"postgres://me@db.local:5432/erp?sslmode=disable", Postgres, "db.local › erp"},
		{"postgresql://me:secret@db.local/erp", Postgres, "db.local › erp"},
		{"postgres:///erp?host=/run/postgresql", Postgres, "/run/postgresql › erp"},
		{"postgres://me@db.local", Postgres, "db.local"},
		{"host=db.local dbname=erp user=me", Postgres, "db.local › erp"},
		{"dbname=erp", Postgres, "localhost › erp"},
		{"sqlserver://sa@sql.local?database=Sales", SQLServer, "sql.local › Sales"},
		{"sqlserver://sa@sql.local/SQLEXPRESS?database=erp", SQLServer, `sql.local\SQLEXPRESS › erp`},
		{"server=sql.local;user id=sa;database=erp", SQLServer, "sql.local › erp"},
		{"Data Source=sql.local;Initial Catalog=erp", SQLServer, "sql.local › erp"},
		{file, SQLite, "app.db"},
	} {
		cfg, err := ParseTarget(c.arg)
		if err != nil || cfg.Driver != c.driver || cfg.Name != c.name {
			t.Errorf("%q: %+v, %v; want %s %q", c.arg, cfg, err, c.driver, c.name)
		}
		if strings.Contains(cfg.Name, "secret") {
			t.Errorf("%q: the password is in the name", c.arg)
		}
	}

	// SQLite paths are made absolute, so tabs and the explorer agree.
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(dir)
	cfg, err := ParseTarget("app.db")
	if err != nil || !filepath.IsAbs(cfg.DSN) {
		t.Fatalf("relative path: %+v, %v", cfg, err)
	}
	// The same file, whatever the path says (on macOS /var is /private/var).
	a, _ := os.Stat(cfg.DSN)
	b, _ := os.Stat(file)
	if a == nil || b == nil || !os.SameFile(a, b) {
		t.Errorf("relative path: %q is not %q", cfg.DSN, file)
	}

	for _, arg := range []string{"", "mysql://me@host/db", filepath.Join(dir, "typo.db"), dir} {
		if cfg, err := ParseTarget(arg); err == nil {
			t.Errorf("%q accepted: %+v", arg, cfg)
		}
	}
}

func TestIsAuthError(t *testing.T) {
	for _, err := range []error{
		&pgconn.PgError{Code: "28P01", Message: "password authentication failed for user \"me\""},
		mssql.Error{Number: 18456, Message: "Login failed for user 'sa'."},
		errors.New("failed to connect to `user=me database=erp`: failed SASL auth: no password"),
	} {
		if !IsAuthError(err) {
			t.Errorf("not an auth error: %v", err)
		}
	}
	for _, err := range []error{nil, errors.New("dial tcp: connection refused"), &pgconn.PgError{Code: "3D000"}} {
		if IsAuthError(err) {
			t.Errorf("an auth error: %v", err)
		}
	}
}
