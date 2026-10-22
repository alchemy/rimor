package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"rimor.dev/internal/db"
)

var cols = []db.ExportColumn{
	{Name: "id", Type: "INT", Numeric: true},
	{Name: "name", Type: "TEXT"},
	{Name: "active", Type: "BOOL"},
	{Name: "doc", Type: "JSONB"},
}

var rows = [][]db.Value{
	{{Text: "1"}, {Text: "Ada, \"the\" first"}, {Text: "true"}, {Text: `{"a": [1, 2]}`}},
	{{Text: "2"}, {Text: "line\nbreak | pipe"}, {Text: "false"}, {Text: "NULL", Null: true}},
	{{Text: "007"}, {Text: "O'Brien \\ é"}, {Text: "NULL", Null: true}, {Text: "not json"}},
}

func write(t *testing.T, o Options, cs []db.ExportColumn, rs [][]db.Value) string {
	t.Helper()
	var b bytes.Buffer
	w, err := New(&b, o, cs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if err := w.Row(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestCSV(t *testing.T) {
	got := write(t, Options{Format: CSV, Separator: ';', Null: `\N`, BOM: true}, cols, rows)
	if !strings.HasPrefix(got, "\xEF\xBB\xBFid;name;active;doc\r\n") {
		t.Errorf("header: %q", got[:40])
	}
	// What a CSV reader makes of it is what was exported.
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(got, "\xEF\xBB\xBF")))
	r.Comma = ';'
	recs, err := r.ReadAll()
	if err != nil || len(recs) != 4 {
		t.Fatalf("read back: %v %q", err, recs)
	}
	if recs[1][1] != `Ada, "the" first` || recs[2][1] != "line\nbreak | pipe" || recs[2][3] != `\N` {
		t.Errorf("values: %q", recs)
	}

	tsv := write(t, Options{Format: TSV, Null: ""}, cols, rows[1:2])
	if tsv != "id\tname\tactive\tdoc\n2\t\"line\nbreak | pipe\"\tfalse\t\n" {
		t.Errorf("TSV: %q", tsv)
	}
}

func TestJSON(t *testing.T) {
	got := write(t, Options{Format: JSON}, cols, rows)
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, got)
	}
	if parsed[0]["id"] != float64(1) || parsed[0]["active"] != true || parsed[0]["name"] != `Ada, "the" first` {
		t.Errorf("first row %v", parsed[0])
	}
	if doc, ok := parsed[0]["doc"].(map[string]any); !ok || len(doc["a"].([]any)) != 2 {
		t.Errorf("a JSON column is not embedded: %v", parsed[0]["doc"])
	}
	if parsed[1]["doc"] != nil || parsed[2]["active"] != nil {
		t.Errorf("NULL is not null: %v %v", parsed[1], parsed[2])
	}
	// 007 is no JSON number: kept as text, its zeros with it.
	if parsed[2]["id"] != "007" || parsed[2]["doc"] != "not json" {
		t.Errorf("third row %v", parsed[2])
	}
	if !strings.HasPrefix(got, "[\n  {\"id\": 1, ") || !strings.HasSuffix(got, "}\n]\n") {
		t.Errorf("layout:\n%s", got)
	}
	if empty := write(t, Options{Format: JSON}, cols, nil); empty != "[]\n" {
		t.Errorf("no rows: %q", empty)
	}

	lines := write(t, Options{Format: JSONLines}, cols, rows)
	ls := strings.Split(strings.TrimSuffix(lines, "\n"), "\n")
	if len(ls) != 3 {
		t.Fatalf("JSON Lines: %q", lines)
	}
	for _, l := range ls {
		if !json.Valid([]byte(l)) {
			t.Errorf("not a JSON line: %s", l)
		}
	}

	// Columns with the same name keep both values.
	dup := write(t, Options{Format: JSONLines}, []db.ExportColumn{{Name: "a"}, {Name: "a"}}, [][]db.Value{{{Text: "x"}, {Text: "y"}}})
	if dup != `{"a": "x", "a_2": "y"}`+"\n" {
		t.Errorf("duplicate names: %s", dup)
	}
}

func TestSQLInserts(t *testing.T) {
	got := write(t, Options{Format: SQL, Driver: db.Postgres, Table: "public.people"}, cols, rows[:2])
	want := `INSERT INTO public.people (id, name, active, doc) VALUES
    (1, 'Ada, "the" first', TRUE, '{"a": [1, 2]}'),
    (2, 'line
break | pipe', FALSE, NULL);
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// Batches of 100 rows: 201 rows make three statements.
	var many [][]db.Value
	for i := range 201 {
		many = append(many, []db.Value{{Text: strconv.Itoa(i)}})
	}
	got = write(t, Options{Format: SQL, Driver: db.SQLite}, []db.ExportColumn{{Name: "order", Type: "INTEGER", Numeric: true}}, many)
	if n := strings.Count(got, "INSERT INTO result (\"order\") VALUES\n"); n != 3 {
		t.Errorf("%d INSERTs for 201 rows:\n%s", n, got[:200])
	}
	if strings.Count(got, ";\n") != 3 || !strings.HasSuffix(got, "    (200);\n") {
		t.Errorf("batches end badly: …%s", got[len(got)-60:])
	}

	// Each dialect's strings and booleans.
	for _, c := range []struct {
		driver db.Driver
		want   string
	}{
		{db.MySQL, `'O''Brien \\ é'`},
		{db.SQLServer, `N'O''Brien \ é'`},
		{db.Postgres, `'O''Brien \ é'`},
	} {
		if got := sqlValue(rows[2][1], cols[1], c.driver); got != c.want {
			t.Errorf("%s: %s, want %s", c.driver, got, c.want)
		}
	}
	if got := sqlValue(db.Value{Text: "true"}, cols[2], db.SQLServer); got != "1" {
		t.Errorf("SQL Server bit: %s", got)
	}
	if got := sqlValue(db.Value{Text: "007"}, cols[0], db.Postgres); got != "'007'" {
		t.Errorf("007 lost its zeros: %s", got)
	}
}

func TestMarkdown(t *testing.T) {
	got := write(t, Options{Format: Markdown}, cols[:2], [][]db.Value{rows[1][:2], {{Text: "3"}, {Null: true, Text: "NULL"}}})
	want := "| id | name |\n|---:|---|\n| 2 | line<br>break \\| pipe |\n| 3 | *NULL* |\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestSeparatorFor(t *testing.T) {
	for loc, want := range map[string]rune{
		"it_IT.UTF-8": ';', "de_DE": ';', "fr_FR.UTF-8@euro": ';', "pt_BR.UTF-8": ';',
		"en_US.UTF-8": ',', "en_GB": ',', "ja_JP.UTF-8": ',',
		"de_CH.UTF-8": ',', "es_MX": ',', "es_ES": ';', "en_ZA": ';', "nl-NL": ';',
	} {
		if got := separatorFor(loc); got != want {
			t.Errorf("%s: %q, want %q", loc, got, want)
		}
	}
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_NUMERIC", "it_IT.UTF-8")
	t.Setenv("LANG", "en_US.UTF-8")
	if got := ListSeparator(); got != ';' && systemListSeparator() == 0 {
		t.Errorf("LC_NUMERIC should win over LANG: %q", got)
	}
	t.Setenv("LC_NUMERIC", "C")
	if got := ListSeparator(); got != ',' && systemListSeparator() == 0 {
		t.Errorf("C locale: %q", got)
	}
}

func TestDecimalComma(t *testing.T) {
	cs := []db.ExportColumn{{Name: "amount", Type: "DECIMAL", Numeric: true}, {Name: "version", Type: "TEXT"}, {Name: "n", Type: "INT", Numeric: true}}
	rs := [][]db.Value{{{Text: "120.5"}, {Text: "1.2.3"}, {Text: "7"}}, {{Text: "1.5e-05"}, {Text: "0.5"}, {Null: true, Text: "NULL"}}}

	// ';' in a decimal-comma locale: the decimals of numeric columns only.
	got := write(t, Options{Format: CSV, Separator: ';', Decimal: ','}, cs, rs)
	if want := "amount;version;n\r\n120,5;1.2.3;7\r\n1,5e-05;0.5;\r\n"; got != want {
		t.Errorf("';' and ',':\n%q\nwant\n%q", got, want)
	}
	// ',' keeps the dot, whatever the locale; so do the other formats and a
	// locale that writes dots.
	for _, o := range []Options{
		{Format: CSV, Separator: ',', Decimal: ','},
		{Format: CSV, Separator: ';', Decimal: '.'},
		{Format: TSV, Separator: ';', Decimal: ','},
	} {
		if got := write(t, o, cs, rs[:1]); strings.Contains(got, "120,5") {
			t.Errorf("%+v wrote a decimal comma: %q", o, got)
		}
	}
	if got := write(t, Options{Format: JSONLines, Separator: ';', Decimal: ','}, cs, rs[:1]); !strings.Contains(got, `"amount": 120.5`) {
		t.Errorf("JSON: %s", got)
	}
}

func TestDecimalSeparator(t *testing.T) {
	if systemDecimalSeparator() != 0 {
		t.Skip("the system's setting decides here")
	}
	t.Setenv("LC_ALL", "")
	for loc, want := range map[string]rune{"it_IT.UTF-8": ',', "de_CH": '.', "en_US.UTF-8": '.', "C": '.'} {
		t.Setenv("LC_NUMERIC", loc)
		if got := DecimalSeparator(); got != want {
			t.Errorf("%s: %q, want %q", loc, got, want)
		}
	}
}

// sqlValue is a value as an INSERT writes it.
func sqlValue(v db.Value, c db.ExportColumn, d db.Driver) string {
	var b bytes.Buffer
	writeSQLValue(&b, v, columnsOf([]db.ExportColumn{c})[0], d)
	return b.String()
}

func TestIsNumber(t *testing.T) {
	for s, want := range map[string]bool{
		"0": true, "7": true, "-12": true, "120.5": true, "1.5e-05": true, "2E+10": true, "0.5": true,
		"": false, "007": false, "1.": false, ".5": false, "+1": false, "NaN": false, "Inf": false,
		"1e": false, "--1": false, "1.2.3": false, "12a": false, "-": false,
	} {
		if got := isNumber(s); got != want {
			t.Errorf("isNumber(%q) = %v", s, got)
		}
	}
}
