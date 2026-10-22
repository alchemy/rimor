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
	// SQL: the table to insert into, quoted and qualified ("result" when
	// empty), and the dialect.
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

// ── Columns and values ──────────────────────────────────────────────────────

// column is what the writers need of a column, worked out once.
type column struct {
	numeric, boolean, json bool
}

func columnsOf(cols []db.ExportColumn) []column {
	out := make([]column, len(cols))
	for i, c := range cols {
		t := strings.ToUpper(c.Type)
		out[i] = column{
			numeric: c.Numeric,
			boolean: t == "BOOL" || t == "BOOLEAN" || t == "BIT",
			json:    t == "JSON" || t == "JSONB",
		}
	}
	return out
}

// kind is what a value is written as in JSON and SQL.
type kind int

const (
	kindText kind = iota
	kindNull
	kindNumber  // a number of a numeric column, written as it is
	kindBoolean // true or false of a boolean column
	kindJSON    // the content of a JSON column
)

func (c column) kind(v db.Value) kind {
	switch {
	case v.Null:
		return kindNull
	case c.numeric && isNumber(v.Text):
		return kindNumber
	case c.boolean && (v.Text == "true" || v.Text == "false"):
		return kindBoolean
	case c.json:
		return kindJSON
	}
	return kindText
}

// isNumber reports text in JSON's number syntax, which SQL takes as it is
// too: no 1., .5, +1, NaN, Inf, or leading zeros (007 would lose them).
func isNumber(s string) bool {
	i := 0
	digits := func() int {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i - start
	}
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case digits() == 0:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if digits() == 0 {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	return i == len(s)
}

// ── CSV and TSV ─────────────────────────────────────────────────────────────

type csvWriter struct {
	w     *csv.Writer
	null  string
	rec   []string
	cols  []column
	comma bool // numeric columns write decimals with a comma
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
	return &csvWriter{w: cw, null: o.Null, rec: make([]string, len(cols)), cols: columnsOf(cols), comma: DecimalComma(o)}, nil
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
		case c.comma && c.cols[i].numeric && isNumber(v.Text):
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
	cols  []column
	keys  []string // the column names as JSON keys, made unique
	lines bool
	n     int
	buf   bytes.Buffer // a row, its capacity kept between rows
	enc   *json.Encoder
}

func newJSON(w io.Writer, o Options, cols []db.ExportColumn) (*jsonWriter, error) {
	j := &jsonWriter{w: w, cols: columnsOf(cols), lines: o.Format == JSONLines}
	j.enc = json.NewEncoder(&j.buf)
	j.enc.SetEscapeHTML(false)
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
		j.value(v, j.cols[i])
	}
	b.WriteByte('}')
	if j.lines {
		b.WriteByte('\n')
	}
	j.n++
	_, err := j.w.Write(b.Bytes())
	return err
}

// value writes a value as JSON: numbers of numeric columns as numbers,
// booleans as booleans, JSON columns as the JSON they hold, the rest (and
// JSON columns holding no JSON) as strings.
func (j *jsonWriter) value(v db.Value, c column) {
	b := &j.buf
	switch c.kind(v) {
	case kindNull:
		b.WriteString("null")
		return
	case kindNumber, kindBoolean:
		b.WriteString(v.Text)
		return
	case kindJSON:
		n := b.Len()
		if json.Compact(b, []byte(v.Text)) == nil {
			return
		}
		b.Truncate(n)
	}
	j.enc.Encode(v.Text) // a quoted string, and a line break to take back
	b.Truncate(b.Len() - 1)
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

// ── SQL INSERTs ─────────────────────────────────────────────────────────────

type sqlWriter struct {
	w      io.Writer
	driver db.Driver
	cols   []column
	head   string // INSERT INTO t (a, b) VALUES
	inRow  int    // rows in the open INSERT
	buf    bytes.Buffer
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
	return &sqlWriter{w: w, driver: o.Driver, cols: columnsOf(cols),
		head: "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES\n"}
}

func (s *sqlWriter) Row(values []db.Value) error {
	b := &s.buf
	b.Reset()
	if s.inRow == 0 {
		b.WriteString(s.head)
	} else {
		b.WriteString(",\n")
	}
	b.WriteString("    (")
	for i, v := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		writeSQLValue(b, v, s.cols[i], s.driver)
	}
	b.WriteByte(')')
	if s.inRow++; s.inRow == InsertBatch {
		b.WriteString(";\n")
		s.inRow = 0
	}
	_, err := s.w.Write(b.Bytes())
	return err
}

func (s *sqlWriter) Close() error {
	if s.inRow == 0 {
		return nil
	}
	_, err := io.WriteString(s.w, ";\n")
	return err
}

// writeSQLValue writes a value as a literal of the dialect.
func writeSQLValue(b *bytes.Buffer, v db.Value, c column, d db.Driver) {
	switch c.kind(v) {
	case kindNull:
		b.WriteString("NULL")
	case kindNumber:
		b.WriteString(v.Text)
	case kindBoolean:
		switch {
		case d == db.SQLServer || d == db.SQLite:
			b.WriteByte("01"[b2i(v.Text == "true")])
		default:
			b.WriteString(strings.ToUpper(v.Text))
		}
	default:
		writeSQLString(b, v.Text, d)
	}
}

// writeSQLString quotes text for the dialect: quotes doubled everywhere,
// backslashes doubled for MySQL (an escape in its strings by default),
// N'…' for SQL Server text beyond ASCII.
func writeSQLString(b *bytes.Buffer, s string, d db.Driver) {
	if d == db.SQLServer && !ascii(s) {
		b.WriteByte('N')
	}
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			b.WriteString("''")
		case c == '\\' && d == db.MySQL:
			b.WriteString(`\\`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── Markdown ────────────────────────────────────────────────────────────────

type markdownWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

// markdownCell keeps a value inside its table cell: | escaped, line breaks
// as <br>.
var markdownCell = strings.NewReplacer("|", `\|`, "\r\n", "<br>", "\n", "<br>")

func newMarkdown(w io.Writer, cols []db.ExportColumn) (*markdownWriter, error) {
	m := &markdownWriter{w: w}
	b := &m.buf
	b.WriteString("|")
	for _, c := range cols {
		b.WriteByte(' ')
		markdownCell.WriteString(b, c.Name)
		b.WriteString(" |")
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
	_, err := m.w.Write(b.Bytes())
	return m, err
}

func (m *markdownWriter) Row(values []db.Value) error {
	b := &m.buf
	b.Reset()
	b.WriteString("|")
	for _, v := range values {
		b.WriteByte(' ')
		if v.Null {
			b.WriteString("*NULL*")
		} else {
			markdownCell.WriteString(b, v.Text)
		}
		b.WriteString(" |")
	}
	b.WriteString("\n")
	_, err := m.w.Write(b.Bytes())
	return err
}

func (m *markdownWriter) Close() error { return nil }
