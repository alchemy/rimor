package db

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// A target is a connection given on the command line, for a session that
// is not saved: rimor postgres://me@host/erp, rimor ./app.db.

// ParseTarget recognises a connection string or an SQLite file and makes
// a connection of it, named after what it points to.
func ParseTarget(arg string) (Config, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return Config{}, errors.New("an empty connection")
	}
	scheme, _, isURL := strings.Cut(arg, "://")
	switch strings.ToLower(scheme) {
	case "postgres", "postgresql":
		return target(Postgres, arg)
	case "sqlserver":
		return target(SQLServer, arg)
	}
	if isURL {
		return Config{}, fmt.Errorf("%s:// is not a connection rimor knows: use postgres://, postgresql:// or sqlserver://", scheme)
	}

	// key=value forms: libpq's separates with spaces, ADO's with semicolons.
	if pairs, ok := pgPairs(arg); ok && hasKey(pairs, "host", "dbname", "user", "port", "hostaddr") {
		return target(Postgres, arg)
	}
	if pairs, ok := adoPairs(arg); ok && hasKey(pairs, "server", "data source", "address", "addr", "initial catalog") {
		return target(SQLServer, arg)
	}

	// Otherwise an SQLite file, which must exist: opening a misspelt name
	// would create an empty database.
	info, err := os.Stat(arg)
	switch {
	case err == nil && info.IsDir():
		return Config{}, fmt.Errorf("%s is a folder, not a database file", arg)
	case errors.Is(err, fs.ErrNotExist):
		return Config{}, fmt.Errorf("%s: no such file, and not a connection string "+
			"(postgres://…, sqlserver://…, host=… dbname=…, server=…;database=…)", arg)
	case err != nil:
		return Config{}, err
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return Config{}, err
	}
	return Config{Name: filepath.Base(abs), Driver: SQLite, DSN: abs}, nil
}

func hasKey(pairs []pair, keys ...string) bool {
	for _, p := range pairs {
		for _, k := range keys {
			if strings.EqualFold(p.key, k) {
				return true
			}
		}
	}
	return false
}

// target names a server connection "host › database" (or just the host).
func target(d Driver, dsn string) (Config, error) {
	host, database := targetParts(d, dsn)
	if host == "" {
		host = "localhost"
	}
	name := host
	if database != "" {
		name += " › " + database
	}
	return Config{Name: name, Driver: d, DSN: dsn}, nil
}

func targetParts(d Driver, dsn string) (host, database string) {
	if u, ok := dsnURL(dsn); ok {
		host = u.Hostname()
		if q := u.Query(); q.Get("host") != "" && host == "" {
			host = q.Get("host") // a socket folder
		}
		database = strings.TrimPrefix(u.Path, "/")
		if d == SQLServer {
			database = u.Query().Get("database")
			if inst := strings.TrimPrefix(u.Path, "/"); inst != "" {
				host += `\` + inst // a named instance
			}
		}
		if unescaped, err := url.PathUnescape(database); err == nil {
			database = unescaped
		}
		return host, database
	}
	var pairs []pair
	if d == Postgres {
		pairs, _ = pgPairs(dsn)
	} else {
		pairs, _ = adoPairs(dsn)
	}
	for _, p := range pairs {
		switch strings.ToLower(p.key) {
		case "host", "server", "data source", "address", "addr":
			host = p.value
		case "dbname", "database", "initial catalog":
			database = p.value
		}
	}
	return host, database
}

// HasPassword reports whether a connection string carries a password.
func HasPassword(d Driver, dsn string) bool {
	_, _, ok := splitPassword(d, dsn)
	return ok
}

// WithPassword puts a password into a connection string without one.
func WithPassword(d Driver, dsn, password string) string {
	return joinPassword(d, dsn, password)
}

// IsAuthError reports a server refusing a login: a wrong or missing
// password, or an unknown user.
func IsAuthError(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "28P01" || pg.Code == "28000"
	}
	var ms mssql.Error
	if errors.As(err, &ms) {
		return ms.Number == 18456
	}
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"password authentication failed", "no password supplied", "login failed for user", "failed sasl auth"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
