// Package export writes a result in formats other tools read: CSV, TSV,
// JSON, JSON Lines, SQL INSERTs and Markdown. Rows come one at a time, so
// a result can be written as it streams.
package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"rimor.dev/internal/db"
)

// Format is an export format.
type Format int

const (
	CSV Format = iota
	TSV
	JSON
	JSONLines
	SQL
	Markdown
)

// Formats lists the formats in the order the dialog offers them.
var Formats = []Format{CSV, TSV, JSON, JSONLines, SQL, Markdown}

func (f Format) String() string {
	return [...]string{"CSV", "TSV", "JSON", "JSON Lines", "SQL", "Markdown"}[f]
}

// Ext is the file extension of the format.
func (f Format) Ext() string {
	return [...]string{".csv", ".tsv", ".json", ".jsonl", ".sql", ".md"}[f]
}

// IsExt reports whether ext is one of the formats' extensions.
func IsExt(ext string) bool {
	for _, f := range Formats {
		if strings.EqualFold(f.Ext(), ext) {
			return true
		}
	}
	return false
}

// Options shape an export.
type Options struct {
	Format    Format
	Separator rune // CSV's: ',' or ';'
	// Decimal is the locale's decimal separator. CSV separated by ';'
	// writes the decimals of numeric columns with it, as spreadsheets of
	// decimal-comma locales expect; everything else keeps the dot.
	Decimal rune
	Null    string // how CSV and TSV write NULL: "", "NULL" or `\N`
	BOM     bool   // CSV: start with a UTF-8 byte order mark, for Excel
	// SQL: the table to insert into, quoted and qualified, and the dialect.
	Table  string
	Driver db.Driver
}

// InsertBatch is how many rows an INSERT carries: fast to load, and well
// under SQL Server's 1,000 rows per VALUES.
const InsertBatch = 100

// Writer takes the rows of a result. Close finishes the document.
type Writer interface {
	Row(values []db.Value) error
	Close() error
}

// New starts an export: it writes what comes before the rows, such as a
// CSV header.
func New(w io.Writer, o Options, cols []db.ExportColumn) (Writer, error) {
	switch o.Format {
	case CSV, TSV:
		return newCSV(w, o, cols)
	case JSON, JSONLines:
		return newJSON(w, o, cols)
	case SQL:
		return newSQL(w, o, cols), nil
	case Markdown:
		return newMarkdown(w, cols)
	}
	return nil, fmt.Errorf("export: unknown format %d", o.Format)
}

// ── CSV and TSV ─────────────────────────────────────────────────────────────

type csvWriter struct {
	w       *csv.Writer
	null    string
	rec     []string
	numeric []bool // columns whose decimals take the comma
}

func newCSV(w io.Writer, o Options, cols []db.ExportColumn) (*csvWriter, error) {
	if o.Format == CSV && o.BOM {
		if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
			return nil, err
		}
	}
	cw := csv.NewWriter(w)
	cw.Comma = o.Separator
	if cw.Comma == 0 {
		cw.Comma = ','
	}
	if o.Format == TSV {
		cw.Comma = '\t'
	} else {
		cw.UseCRLF = true // RFC 4180
	}
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.Name
	}
	if err := cw.Write(header); err != nil {
		return nil, err
	}
	c := &csvWriter{w: cw, null: o.Null, rec: make([]string, len(cols))}
	if DecimalComma(o) {
		c.numeric = make([]bool, len(cols))
		for i, col := range cols {
			c.numeric[i] = col.Numeric
		}
	}
	return c, nil
}

// DecimalComma reports whether an export writes decimals with a comma: a
// CSV separated by ';' in a locale that writes them so. A ','-separated
// file keeps the dot, as do numbers in other formats.
func DecimalComma(o Options) bool {
	return o.Format == CSV && o.Separator == ';' && o.Decimal == ','
}

func (c *csvWriter) Row(values []db.Value) error {
	for i, v := range values {
		c.rec[i] = v.Text
		switch {
		case v.Null:
			c.rec[i] = c.null
		case c.numeric != nil && c.numeric[i] && isNumber(v.Text):
			// Only numbers of numeric columns: a code or a version in a
			// text column keeps its dots.
			c.rec[i] = strings.Replace(v.Text, ".", ",", 1)
		}
	}
	return c.w.Write(c.rec)
}

func (c *csvWriter) Close() error {
	c.w.Flush()
	return c.w.Error()
}

// ── JSON and JSON Lines ─────────────────────────────────────────────────────

type jsonWriter struct {
	w     io.Writer
	cols  []db.ExportColumn
	keys  []string // the column names as JSON keys, made unique
	lines bool
	n     int
	buf   strings.Builder
}

func newJSON(w io.Writer, o Options, cols []db.ExportColumn) (*jsonWriter, error) {
	j := &jsonWriter{w: w, cols: cols, lines: o.Format == JSONLines}
	seen := map[string]int{}
	for _, c := range cols {
		name := c.Name
		if seen[name]++; seen[name] > 1 {
			name += "_" + strconv.Itoa(seen[c.Name]) // SELECT a, a: keys must differ
		}
		key, _ := json.Marshal(name)
		j.keys = append(j.keys, string(key))
	}
	if !j.lines {
		if _, err := io.WriteString(w, "["); err != nil {
			return nil, err
		}
	}
	return j, nil
}

func (j *jsonWriter) Row(values []db.Value) error {
	b := &j.buf
	b.Reset()
	switch {
	case j.lines:
	case j.n == 0:
		b.WriteString("\n  ")
	default:
		b.WriteString(",\n  ")
	}
	b.WriteByte('{')
	for i, v := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(j.keys[i])
		b.WriteString(": ")
		b.WriteString(jsonValue(v, j.cols[i]))
	}
	b.WriteByte('}')
	if j.lines {
		b.WriteByte('\n')
	}
	j.n++
	_, err := io.WriteString(j.w, b.String())
	return err
}

func (j *jsonWriter) Close() error {
	if j.lines {
		return nil
	}
	end := "\n]\n"
	if j.n == 0 {
		end = "]\n"
	}
	_, err := io.WriteString(j.w, end)
	return err
}

// jsonValue writes a value as JSON: numbers of numeric columns as numbers,
// booleans as booleans, JSON columns as the JSON they hold, the rest as
// strings.
func jsonValue(v db.Value, c db.ExportColumn) string {
	switch {
	case v.Null:
		return "null"
	case c.Numeric && isNumber(v.Text):
		return v.Text
	case boolType(c.Type) && (v.Text == "true" || v.Text == "false"):
		return v.Text
	case jsonType(c.Type) && json.Valid([]byte(v.Text)):
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(v.Text)); err == nil {
			return compact.String()
		}
	}
	s, _ := json.Marshal(v.Text)
	return string(s)
}

// isNumber reports text that JSON and SQL take as a number as it is.
func isNumber(s string) bool {
	if s == "" {
		return false
	}
	// JSON's number syntax, which SQL takes too: no 1., .5, +1, NaN, Inf
	// or leading zeros (007 would lose them).
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return false
	}
	return json.Valid([]byte(s))
}

func boolType(t string) bool {
	switch strings.ToUpper(t) {
	case "BOOL", "BOOLEAN", "BIT":
		return true
	}
	return false
}

func jsonType(t string) bool {
	switch strings.ToUpper(t) {
	case "JSON", "JSONB":
		return true
	}
	return false
}

// ── SQL INSERTs ─────────────────────────────────────────────────────────────

type sqlWriter struct {
	w     io.Writer
	o     Options
	cols  []db.ExportColumn
	head  string // INSERT INTO t (a, b) VALUES
	inRow int    // rows in the open INSERT
	buf   strings.Builder
}

func newSQL(w io.Writer, o Options, cols []db.ExportColumn) *sqlWriter {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = db.QuoteName(o.Driver, c.Name)
	}
	table := o.Table
	if table == "" {
		table = "result"
	}
	return &sqlWriter{w: w, o: o, cols: cols,
		head: "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES\n"}
}

func (s *sqlWriter) Row(values []db.Value) error {
	b := &s.buf
	b.Reset()
	switch {
	case s.inRow == 0:
		b.WriteString(s.head)
	default:
		b.WriteString(",\n")
	}
	b.WriteString("    (")
	for i, v := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(sqlValue(v, s.cols[i], s.o.Driver))
	}
	b.WriteByte(')')
	s.inRow++
	if s.inRow == InsertBatch {
		b.WriteString(";\n")
		s.inRow = 0
	}
	_, err := io.WriteString(s.w, b.String())
	return err
}

func (s *sqlWriter) Close() error {
	if s.inRow == 0 {
		return nil
	}
	_, err := io.WriteString(s.w, ";\n")
	return err
}

// sqlValue writes a value as a literal of the dialect.
func sqlValue(v db.Value, c db.ExportColumn, d db.Driver) string {
	switch {
	case v.Null:
		return "NULL"
	case c.Numeric && isNumber(v.Text):
		return v.Text
	case boolType(c.Type) && (v.Text == "true" || v.Text == "false"):
		switch d {
		case db.SQLServer, db.SQLite:
			if v.Text == "true" {
				return "1"
			}
			return "0"
		}
		return strings.ToUpper(v.Text)
	}
	return sqlString(v.Text, d)
}

// sqlString quotes text for the dialect: quotes doubled everywhere,
// backslashes doubled for MySQL (an escape in its strings by default),
// N'…' for SQL Server text beyond ASCII.
func sqlString(s string, d db.Driver) string {
	s = strings.ReplaceAll(s, "'", "''")
	if d == db.MySQL {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	if d == db.SQLServer {
		for _, r := range s {
			if r > 127 {
				return "N'" + s + "'"
			}
		}
	}
	return "'" + s + "'"
}

// ── Markdown ────────────────────────────────────────────────────────────────

type markdownWriter struct {
	w   io.Writer
	buf strings.Builder
}

func newMarkdown(w io.Writer, cols []db.ExportColumn) (*markdownWriter, error) {
	var b strings.Builder
	b.WriteString("|")
	for _, c := range cols {
		b.WriteString(" " + markdownCell(c.Name) + " |")
	}
	b.WriteString("\n|")
	for _, c := range cols {
		if c.Numeric {
			b.WriteString("---:|") // numbers right-aligned, as in the grid
		} else {
			b.WriteString("---|")
		}
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return &markdownWriter{w: w}, err
}

func (m *markdownWriter) Row(values []db.Value) error {
	b := &m.buf
	b.Reset()
	b.WriteString("|")
	for _, v := range values {
		text := markdownCell(v.Text)
		if v.Null {
			text = "*NULL*"
		}
		b.WriteString(" " + text + " |")
	}
	b.WriteString("\n")
	_, err := io.WriteString(m.w, b.String())
	return err
}

func (m *markdownWriter) Close() error { return nil }

// markdownCell keeps a value inside its table cell: | escaped, line breaks
// as <br>.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", "<br>")
	return strings.ReplaceAll(s, "\n", "<br>")
}
