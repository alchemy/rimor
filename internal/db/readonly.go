package db

import (
	"fmt"
	"strings"
	"unicode"
)

// writeWords start or mark statements that change data, schema or
// permissions, or run code that might (EXEC); INTO catches SELECT … INTO.
var writeWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true,
	"DROP": true, "ALTER": true, "CREATE": true, "GRANT": true, "REVOKE": true, "DENY": true,
	"EXEC": true, "EXECUTE": true, "INTO": true, "BULK": true, "RESTORE": true, "BACKUP": true,
	"DBCC": true, "RECONFIGURE": true, "SHUTDOWN": true, "KILL": true,
}

// alsoWrites are PostgreSQL statements that write, which T-SQL lacks.
var alsoWrites = map[string]bool{"CALL": true, "COPY": true, "VACUUM": true, "REINDEX": true, "CLUSTER": true, "REFRESH": true}

// MayWrite returns the first word that suggests the SQL writes (data,
// schema, permissions, or code that might), or "". Like CheckReadOnly it
// errs on the side of warning.
func MayWrite(query string) string {
	for _, w := range sqlWords(query) {
		if writeWords[w] || alsoWrites[w] {
			return w
		}
	}
	return ""
}

// CheckReadOnly refuses SQL that may write, for read-only connections on
// SQL Server, which has no session-level read-only mode. It looks at
// every word outside comments, strings and quoted names, so it errs on the
// side of refusing.
func CheckReadOnly(query string) error {
	for _, w := range sqlWords(query) {
		if writeWords[w] {
			return fmt.Errorf("this connection is read-only: %s is not allowed", w)
		}
	}
	return nil
}

// sqlWords returns the upper-cased bare words of a T-SQL text.
func sqlWords(s string) []string {
	var words []string
	r := []rune(s)
	for i := 0; i < len(r); {
		switch c := r[i]; {
		case c == '-' && i+1 < len(r) && r[i+1] == '-':
			for i < len(r) && r[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			i += 2
			for i+1 < len(r) && !(r[i] == '*' && r[i+1] == '/') {
				i++
			}
			i += 2
		case c == '\'' || c == '"' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			i++
			for i < len(r) {
				if r[i] == end {
					if i+1 < len(r) && r[i+1] == end { // doubled: escaped
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
		case unicode.IsLetter(c) || c == '_' || c == '@' || c == '#':
			start := i
			for i < len(r) && (unicode.IsLetter(r[i]) || unicode.IsDigit(r[i]) || r[i] == '_' || r[i] == '@' || r[i] == '#' || r[i] == '$') {
				i++
			}
			words = append(words, strings.ToUpper(string(r[start:i])))
		default:
			i++
		}
	}
	return words
}
