package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// MySQL and MariaDB, through go-sql-driver/mysql. Connection strings come
// in the driver's own form, user:pass@tcp(host:3306)/db?param=value, or as
// a URL, mysql://user:pass@host:3306/db?param=value, which the driver does
// not read and rimor turns into the same configuration.

// mysqlConfig parses a connection string; database, when set, replaces
// the one it names.
func mysqlConfig(dsn, database string) (*mysql.Config, error) {
	dsn = strings.TrimSpace(dsn)
	var cfg *mysql.Config
	if u, ok := mysqlURL(dsn); ok {
		addr := u.Host
		if u.Port() == "" && addr != "" {
			addr = net.JoinHostPort(u.Hostname(), "3306")
		}
		// The driver parses its own options; the password is set apart, so
		// that no character of it can upset the parsing.
		native := url.PathEscape(u.User.Username()) + "@tcp(" + addr + ")/" + strings.TrimPrefix(u.Path, "/")
		if u.RawQuery != "" {
			native += "?" + u.RawQuery
		}
		var err error
		if cfg, err = mysql.ParseDSN(native); err != nil {
			return nil, err
		}
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	} else {
		var err error
		if cfg, err = mysql.ParseDSN(dsn); err != nil {
			return nil, err
		}
	}
	if database != "" {
		cfg.DBName = database
	}
	// Batches of several statements, as in the other engines.
	cfg.MultiStatements = true
	// The driver asks for utf8mb4_general_ci, which MySQL 8 tables
	// (utf8mb4_0900_ai_ci) cannot compare with: "Illegal mix of
	// collations" on WHERE status = @var. SET NAMES utf8mb4 alone takes
	// the server's own default instead, unless the user chose one.
	if !strings.Contains(dsn, "charset=") && !strings.Contains(dsn, "collation=") {
		if err := mysql.Charset("utf8mb4", "")(cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// mysqlURL parses a mysql:// or mariadb:// connection string.
func mysqlURL(dsn string) (*url.URL, bool) {
	scheme, _, ok := strings.Cut(dsn, "://")
	if !ok {
		return nil, false
	}
	switch strings.ToLower(scheme) {
	case "mysql", "mariadb":
	default:
		return nil, false
	}
	u, err := url.Parse(dsn)
	return u, err == nil
}

// openMySQL opens a database of a MySQL connection.
func openMySQL(cfg Config, database string) (*sql.DB, error) {
	mc, err := mysqlConfig(cfg.DSN, database)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, err
	}
	if cfg.ReadOnly {
		connector = readOnlyConnector{connector}
	}
	return sql.OpenDB(connector), nil
}

// readOnlyConnector makes every connection's transactions read-only, which
// the server enforces: writes, DDL included, fail with error 1792. It is
// one statement for MySQL and MariaDB, whose variables are named apart.
type readOnlyConnector struct{ driver.Connector }

func (c readOnlyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("mysql: cannot make the session read-only")
	}
	if _, err := ex.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY", nil); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
