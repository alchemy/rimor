// Package db holds saved connections and the per-driver catalog
// introspection that feeds the explorer tree.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "modernc.org/sqlite"
)

// Driver identifies a supported database engine.
type Driver string

const (
	Postgres  Driver = "postgres"
	SQLite    Driver = "sqlite"
	SQLServer Driver = "sqlserver"
)

// Drivers lists the supported engines in display order.
var Drivers = []Driver{Postgres, SQLite, SQLServer}

func (d Driver) Label() string {
	switch d {
	case Postgres:
		return "PostgreSQL"
	case SQLite:
		return "SQLite"
	case SQLServer:
		return "SQL Server"
	}
	return string(d)
}

// Short is a compact tag for tight spaces such as the tree.
func (d Driver) Short() string {
	switch d {
	case Postgres:
		return "pg"
	case SQLite:
		return "sqlite"
	case SQLServer:
		return "mssql"
	}
	return string(d)
}

// DSNHint is an example connection string for the driver.
func (d Driver) DSNHint() string {
	switch d {
	case Postgres:
		return "postgres://user:pass@localhost:5432/db?sslmode=disable"
	case SQLite:
		return "/path/to/database.db"
	case SQLServer:
		return "sqlserver://user:pass@localhost:1433?database=db"
	}
	return ""
}

func (d Driver) sqlName() string {
	switch d {
	case Postgres:
		return "pgx"
	case SQLite:
		return "sqlite"
	case SQLServer:
		return "sqlserver"
	}
	return string(d)
}

// Config is a saved connection.
type Config struct {
	Name   string `json:"name"`
	Driver Driver `json:"driver"`
	DSN    string `json:"dsn"`
}

// HasDatabases reports whether one connection reaches several databases.
func (d Driver) HasDatabases() bool { return d != SQLite }

// Open connects to the connection's default database and verifies the
// connection is alive.
func Open(ctx context.Context, cfg Config) (*sql.DB, error) {
	return OpenDatabase(ctx, cfg, "")
}

// OpenDatabase connects to a specific database of the connection's server;
// an empty name means the default database of the DSN.
func OpenDatabase(ctx context.Context, cfg Config, database string) (*sql.DB, error) {
	dsn := strings.TrimSpace(cfg.DSN)
	var conn *sql.DB
	switch cfg.Driver {
	case SQLite:
		// The driver silently creates missing files; an explorer should not.
		if _, err := os.Stat(dsn); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no such file: %s", dsn)
		} else if err != nil {
			return nil, err
		}
	case Postgres:
		if database != "" {
			pc, err := pgx.ParseConfig(dsn)
			if err != nil {
				return nil, err
			}
			pc.Database = database
			conn = stdlib.OpenDB(*pc)
		}
	case SQLServer:
		if database != "" {
			var err error
			if dsn, err = mssqlWithDatabase(dsn, database); err != nil {
				return nil, err
			}
		}
	}
	if conn == nil {
		var err error
		if conn, err = sql.Open(cfg.Driver.sqlName(), dsn); err != nil {
			return nil, err
		}
	}
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Kind is the type of a catalog object.
type Kind int

const (
	KindFolder Kind = iota
	KindDatabase
	KindSchema
	KindTable
	KindView
	KindMatView
	KindFunction
	KindProcedure
	KindSequence
	KindIndex
	KindTrigger
	KindColumn
)

// Object is one node of the catalog hierarchy.
type Object struct {
	Kind   Kind
	Name   string
	Detail string // secondary text: column type, index flavour, …

	// Location of the object in the catalog. Database is empty for the
	// connection's default database.
	Database string
	Schema   string
	Table    string

	// Contains is the kind of the children of a folder.
	Contains Kind
	// Primary marks primary key columns and indexes.
	Primary bool
	// Empty marks schemas with nothing the login can see: no objects, user
	// types or XML schema collections. The explorer hides them unless asked.
	// The user's default schema is never empty.
	Empty bool
}

// schemaObject builds a schema from a (name, "empty" or "") row.
func schemaObject(name, empty string) Object {
	return Object{Kind: KindSchema, Name: name, Schema: name, Empty: empty == "empty"}
}

// Expandable reports whether the object has children.
func (o Object) Expandable() bool {
	switch o.Kind {
	case KindFolder, KindDatabase, KindSchema, KindTable, KindView, KindMatView:
		return true
	}
	return false
}

func folder(name string, contains Kind, schema, table string) Object {
	return Object{Kind: KindFolder, Name: name, Contains: contains, Schema: schema, Table: table}
}

// Introspector lists the children of catalog objects. A nil parent asks for
// the top level of the connection: its databases, where the engine has
// several. conn is connected to the parent's database.
type Introspector interface {
	Children(ctx context.Context, conn *sql.DB, parent *Object) ([]Object, error)
}

// IntrospectorFor returns the catalog reader for a driver.
func IntrospectorFor(d Driver) Introspector {
	switch d {
	case Postgres:
		return postgres{}
	case SQLite:
		return sqlite{}
	case SQLServer:
		return sqlserver{}
	}
	return nil
}

// queryObjects runs a query whose rows are (name, detail) pairs and turns
// each row into an Object built by mk.
func queryObjects(ctx context.Context, conn *sql.DB, mk func(name, detail string) Object, query string, args ...any) ([]Object, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Object
	for rows.Next() {
		var name, detail string
		if err := rows.Scan(&name, &detail); err != nil {
			return nil, err
		}
		out = append(out, mk(name, detail))
	}
	return out, rows.Err()
}

// queryColumns scans rows of (name, type, primary) into column objects.
func queryColumns(ctx context.Context, conn *sql.DB, query string, args ...any) ([]Object, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Object
	for rows.Next() {
		var o Object
		if err := rows.Scan(&o.Name, &o.Detail, &o.Primary); err != nil {
			return nil, err
		}
		o.Kind = KindColumn
		out = append(out, o)
	}
	return out, rows.Err()
}

// queryIndexes scans rows of (name, unique, primary) into index objects.
func queryIndexes(ctx context.Context, conn *sql.DB, query string, args ...any) ([]Object, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Object
	for rows.Next() {
		var o Object
		var unique bool
		if err := rows.Scan(&o.Name, &unique, &o.Primary); err != nil {
			return nil, err
		}
		o.Kind = KindIndex
		switch {
		case o.Primary:
			o.Detail = "primary"
		case unique:
			o.Detail = "unique"
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
