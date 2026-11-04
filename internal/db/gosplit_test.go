package db

import (
	"strings"
	"testing"
)

func TestSplitGO(t *testing.T) {
	batches := func(script string) []string {
		var out []string
		for _, b := range splitGO(script) {
			out = append(out, strings.TrimSpace(b.Text)+"@"+string(rune('0'+b.Line)))
		}
		return out
	}
	for _, c := range []struct {
		script string
		want   []string
	}{
		{"SELECT 1", []string{"SELECT 1@0"}},
		{"SET ANSI_NULLS ON;\nGO\nALTER PROCEDURE p AS SELECT 1\n", []string{"SET ANSI_NULLS ON;@0", "ALTER PROCEDURE p AS SELECT 1@2"}},
		{"a\r\ngo\r\nb", []string{"a@0", "b@2"}},                                     // lowercase, CRLF
		{"a\n  GO  -- next\nb\nGO\n\nGO\n", []string{"a@0", "b@2"}},                  // comment after GO; empty batches
		{"PRINT 'x'\nGO 3\n", []string{"PRINT 'x'@0", "PRINT 'x'@0", "PRINT 'x'@0"}}, // repeated
		{"SELECT 'a\nGO\nb'\nGO\nc", []string{"SELECT 'a\nGO\nb'@0", "c@4"}},         // inside a string
		{"/* a\n/* nested\nGO\n*/ still\nGO\n*/ x\nGO\ny", []string{"/* a\n/* nested\nGO\n*/ still\nGO\n*/ x@0", "y@7"}},
		{"SELECT [a\nGO\n]]b]\nGO\nc", []string{"SELECT [a\nGO\n]]b]@0", "c@4"}}, // inside a bracketed name
		{"-- 'not a string\nGO\nb", []string{"-- 'not a string@0", "b@2"}},       // a quote in a line comment
		{"SELECT 1 AS go\nGOTO x\nGO", []string{"SELECT 1 AS go\nGOTO x@0"}},     // GO only alone on its line
		{"\n\nSELECT 1", []string{"SELECT 1@2"}},                                 // the line counts the leading blanks
	} {
		got := batches(c.script)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q:\n got %q\nwant %q", c.script, got, c.want)
		}
	}
	if n := len(splitGO("x\nGO 99999")); n != maxGORepeat {
		t.Errorf("GO 99999 made %d batches", n)
	}
}
