package db

import (
	"regexp"
	"strconv"
	"strings"
)

// SQL Server scripts separate batches with GO on a line of its own, as
// SQL Server Management Studio and sqlcmd do; GO is no T-SQL, so the
// client splits the script and sends each batch in turn. Some statements
// must open a batch (CREATE or ALTER PROCEDURE, VIEW, FUNCTION, TRIGGER).

// sqlBatch is one batch of a script.
type sqlBatch struct {
	Text string
	Line int // the script line it starts on, from 0
}

// goLine is a separator: GO, optionally a repeat count, optionally a
// comment.
var goLine = regexp.MustCompile(`(?i)^\s*go(?:\s+(\d+))?\s*(?:--.*)?$`)

// maxGORepeat bounds GO n.
const maxGORepeat = 1000

// splitGO splits a script at its GO lines. A GO inside a string, a
// bracketed or quoted name, or a comment is no separator. Batches of
// nothing but blanks are dropped; GO n repeats the batch before it n
// times. A script without GO is one batch.
func splitGO(script string) []sqlBatch {
	var out []sqlBatch
	var cur strings.Builder
	start := 0
	var st scanState
	lines := strings.SplitAfter(script, "\n")
	for i, line := range lines {
		bare := strings.TrimRight(line, "\r\n")
		if st.normal() {
			if m := goLine.FindStringSubmatch(bare); m != nil {
				times := 1
				if m[1] != "" {
					times, _ = strconv.Atoi(m[1])
					times = min(max(times, 1), maxGORepeat)
				}
				if text := cur.String(); strings.TrimSpace(text) != "" {
					for range times {
						out = append(out, sqlBatch{Text: text, Line: start})
					}
				}
				cur.Reset()
				start = i + 1
				continue
			}
		}
		if cur.Len() == 0 && strings.TrimSpace(line) == "" {
			start = i + 1 // leading blank lines belong to no batch
			continue
		}
		cur.WriteString(line)
		st.scan(line)
	}
	if text := cur.String(); strings.TrimSpace(text) != "" {
		out = append(out, sqlBatch{Text: text, Line: start})
	}
	return out
}

// scanState follows T-SQL's quoting across lines: strings ('…' with ”),
// quoted names ("…" and […] with ]]), comments (-- to the end of the line,
// and /* */, which nest).
type scanState struct {
	quote   byte // ' " or ] while inside one
	comment int  // depth of /* */
}

func (s *scanState) normal() bool { return s.quote == 0 && s.comment == 0 }

func (s *scanState) scan(line string) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		next := byte(0)
		if i+1 < len(line) {
			next = line[i+1]
		}
		switch {
		case s.comment > 0:
			switch {
			case c == '*' && next == '/':
				s.comment--
				i++
			case c == '/' && next == '*':
				s.comment++
				i++
			}
		case s.quote != 0:
			if c == s.quote {
				if next == s.quote {
					i++ // '' "" ]] stand for one
				} else {
					s.quote = 0
				}
			}
		case c == '-' && next == '-':
			return // the rest of the line is a comment
		case c == '/' && next == '*':
			s.comment++
			i++
		case c == '\'' || c == '"':
			s.quote = c
		case c == '[':
			s.quote = ']'
		}
	}
}
