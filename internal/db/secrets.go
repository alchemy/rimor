package db

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/zalando/go-keyring"
)

// Passwords live in the system keychain: Keychain on macOS, Credential
// Manager on Windows, the Secret Service (GNOME Keyring, KWallet) on
// Linux. connections.json keeps each connection string without its
// password, marked so that loading puts it back. Where there is no
// keychain, passwords stay in the file, as before.

// Secrets stores one password per connection.
type Secrets interface {
	Get(connection string) (string, error)
	Set(connection, password string) error
	Delete(connection string) error
}

// secretInKeychain marks a connection whose password is in the keychain.
const secretInKeychain = "keychain"

// Keychain is the system keychain. Entries are named after the connection,
// under a service named after the folder of connections.json, so that two
// configurations (a test one, say) never share passwords.
type Keychain struct{ Service string }

func (k Keychain) Get(name string) (string, error) { return keyring.Get(k.Service, name) }
func (k Keychain) Set(name, password string) error { return keyring.Set(k.Service, name, password) }

// Delete removes an entry; one that is not there is not an error.
func (k Keychain) Delete(name string) error {
	if err := keyring.Delete(k.Service, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// KeychainError reports that the keychain could not take a password, so
// connections.json was saved with it, as before keychain support. The
// save itself succeeded.
type KeychainError struct{ Err error }

func (e *KeychainError) Error() string {
	return "passwords stay in connections.json: the system keychain is not available (" + e.Err.Error() + ")"
}

func (e *KeychainError) Unwrap() error { return e.Err }

// splitPassword removes the password from a connection string. ok is
// false when there is none, or the string has a form this does not
// handle, in which case it is left alone.
func splitPassword(d Driver, dsn string) (clean, password string, ok bool) {
	dsn = strings.TrimSpace(dsn)
	if d == SQLite {
		return dsn, "", false
	}
	if u, ok := dsnURL(dsn); ok {
		password, set := u.User.Password()
		if !set || password == "" {
			return dsn, "", false
		}
		u.User = url.User(u.User.Username())
		return u.String(), password, true
	}
	switch d {
	case Postgres:
		return splitPair(dsn, pgPairs, " ", "password")
	case SQLServer:
		return splitPair(dsn, adoPairs, "; ", "password", "pwd")
	case MySQL:
		return splitMySQL(dsn)
	}
	return dsn, "", false
}

// mysqlCredentials finds user[:password] in the driver's own form,
// user:pass@tcp(host)/db, as go-sql-driver does: the last "/" ends the
// address, the last "@" before it ends the credentials, and the first
// ":" in them starts the password.
func mysqlCredentials(dsn string) (at, colon int, ok bool) {
	slash := strings.LastIndexByte(dsn, '/')
	if slash < 0 {
		return 0, 0, false
	}
	at = strings.LastIndexByte(dsn[:slash], '@')
	if at < 0 {
		return 0, 0, false
	}
	return at, strings.IndexByte(dsn[:at], ':'), true
}

func splitMySQL(dsn string) (string, string, bool) {
	at, colon, ok := mysqlCredentials(dsn)
	if !ok || colon < 0 || colon+1 == at {
		return dsn, "", false
	}
	return dsn[:colon] + dsn[at:], dsn[colon+1 : at], true
}

// dsnURL parses a connection string in URL form: postgres://,
// postgresql:// or sqlserver://.
func dsnURL(dsn string) (*url.URL, bool) {
	scheme, _, ok := strings.Cut(dsn, "://")
	if !ok {
		return nil, false
	}
	switch strings.ToLower(scheme) {
	case "postgres", "postgresql", "sqlserver", "mysql", "mariadb":
	default:
		return nil, false
	}
	u, err := url.Parse(dsn)
	return u, err == nil
}

// joinPassword puts a password back into a connection string that
// splitPassword cleaned.
func joinPassword(d Driver, dsn, password string) string {
	if password == "" {
		return dsn
	}
	if u, ok := dsnURL(dsn); ok {
		u.User = url.UserPassword(u.User.Username(), password)
		return u.String()
	}
	switch d {
	case Postgres:
		quoted := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(password)
		return strings.TrimSpace(dsn + " password='" + quoted + "'")
	case MySQL:
		if at, colon, ok := mysqlCredentials(dsn); ok && colon < 0 {
			return dsn[:at] + ":" + password + dsn[at:]
		}
		return dsn
	case SQLServer:
		sep := ";"
		if dsn == "" || strings.HasSuffix(strings.TrimSpace(dsn), ";") {
			sep = ""
		}
		return dsn + sep + `password="` + strings.ReplaceAll(password, `"`, `""`) + `"`
	}
	return dsn
}

// pair is one key=value setting of a connection string, with the text it
// came from.
type pair struct {
	key, value string
	start, end int // the setting's span in the string
}

// splitPair removes every setting named one of keys, each with the
// separators (seps) that follow it, and returns the last one's value,
// which is the one drivers use; the rest is kept as written.
func splitPair(dsn string, parse func(string) ([]pair, bool), seps string, keys ...string) (string, string, bool) {
	pairs, ok := parse(dsn)
	if !ok {
		return dsn, "", false
	}
	var b strings.Builder
	password, found, from := "", false, 0
	for _, p := range pairs {
		if !slices.ContainsFunc(keys, func(k string) bool { return strings.EqualFold(p.key, k) }) {
			continue
		}
		end := p.end
		for end < len(dsn) && strings.IndexByte(seps, dsn[end]) >= 0 {
			end++
		}
		b.WriteString(dsn[from:p.start])
		from, password, found = end, p.value, true
	}
	if !found || password == "" {
		return dsn, "", false
	}
	b.WriteString(dsn[from:])
	return strings.TrimSpace(b.String()), password, true
}

// pgPairs parses libpq's key=value form: settings separated by spaces,
// values optionally in single quotes with backslash escapes.
func pgPairs(s string) ([]pair, bool) {
	var out []pair
	i := 0
	for {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i == len(s) {
			return out, len(out) > 0
		}
		start := i
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, false
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		for i < len(s) && s[i] == ' ' {
			i++
		}
		var value strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			for ; i < len(s) && s[i] != '\''; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				value.WriteByte(s[i])
			}
			if i == len(s) {
				return nil, false // unterminated quote
			}
			i++
		} else {
			for ; i < len(s) && s[i] != ' '; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				value.WriteByte(s[i])
			}
		}
		if key == "" || strings.ContainsAny(key, " '") {
			return nil, false
		}
		out = append(out, pair{key: key, value: value.String(), start: start, end: i})
	}
}

// adoPairs parses the ADO form SQL Server uses, the way go-mssqldb does:
// settings separated by semicolons outside double quotes, values trimmed,
// and a value in double quotes unquoted, "" standing for ". The odbc:
// form, with its braces, is not read.
func adoPairs(s string) ([]pair, bool) {
	if strings.HasPrefix(s, "odbc:") {
		return nil, false
	}
	var out []pair
	start, inQuotes := 0, false
	add := func(end int) {
		part := s[start:end]
		key, value, hasValue := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		value = strings.TrimSpace(value)
		if hasValue && len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = strings.ReplaceAll(value[1:len(value)-1], `""`, `"`)
		}
		out = append(out, pair{key: key, value: value, start: start, end: end})
	}
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"' && inQuotes && i+1 < len(s) && s[i+1] == '"':
			i++
		case s[i] == '"':
			inQuotes = !inQuotes
		case s[i] == ';' && !inQuotes:
			add(i)
			start = i + 1
		}
	}
	if start < len(s) {
		add(len(s))
	}
	return out, len(out) > 0
}

// withSecret is the connection as loaded: its password back in the
// connection string, or the reason it could not be.
func withSecret(c Config, secrets Secrets) Config {
	if c.Secret != secretInKeychain {
		return c
	}
	if secrets == nil {
		c.SecretErr = errors.New("the password is in the system keychain, which is not in use")
		return c
	}
	password, err := secrets.Get(c.Name)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			err = errors.New("no entry for it")
		}
		c.SecretErr = fmt.Errorf("could not read the password from the system keychain: %w", err)
		return c
	}
	c.DSN = joinPassword(c.Driver, c.DSN, password)
	return c
}
