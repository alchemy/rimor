package editor

import (
	"image/color"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// class is a highlighting category; the theme gives each one a colour.
type class uint8

const (
	clsText class = iota
	clsKeyword
	clsType
	clsFunction
	clsString
	clsNumber
	clsComment
	clsOperator
	clsPunctuation
	clsVariable
	clsQuoted // quoted identifiers: "name", [name]
	classCount
)

// Theme colours the editor. Nil colours use the terminal default.
type Theme struct {
	Text, Keyword, Type, Function, String, Number    color.Color
	Comment, Operator, Punctuation, Variable, Quoted color.Color

	LineNumber, CurrentLineNumber, Selection, Placeholder color.Color

	// ReverseSelection shows the selection in reverse video instead of the
	// Selection background, for terminals without colour.
	ReverseSelection bool
}

func (t Theme) color(c class) color.Color {
	switch c {
	case clsKeyword:
		return t.Keyword
	case clsType:
		return t.Type
	case clsFunction:
		return t.Function
	case clsString:
		return t.String
	case clsNumber:
		return t.Number
	case clsComment:
		return t.Comment
	case clsOperator:
		return t.Operator
	case clsPunctuation:
		return t.Punctuation
	case clsVariable:
		return t.Variable
	case clsQuoted:
		return t.Quoted
	}
	return t.Text
}

// Language names a SQL dialect for highlighting.
type Language string

const (
	SQL        Language = "sql"
	PostgreSQL Language = "postgres"
	TSQL       Language = "tsql"
)

func lexerFor(lang Language) chroma.Lexer {
	l := lexers.Get(string(lang))
	if l == nil {
		l = lexers.Get("sql")
	}
	return chroma.Coalesce(l)
}

func classify(t chroma.TokenType) class {
	switch {
	case t.InCategory(chroma.Comment):
		return clsComment
	case t == chroma.KeywordType, t == chroma.NameBuiltinPseudo:
		return clsType
	case t == chroma.OperatorWord:
		return clsKeyword
	case t.InCategory(chroma.Keyword):
		return clsKeyword
	case t == chroma.LiteralStringName:
		return clsQuoted
	case t.InSubCategory(chroma.NameVariable):
		return clsVariable
	case t == chroma.NameBuiltin, t.InSubCategory(chroma.NameFunction):
		return clsFunction
	case t.InSubCategory(chroma.LiteralString):
		return clsString
	case t.InSubCategory(chroma.LiteralNumber):
		return clsNumber
	case t.InCategory(chroma.Operator):
		return clsOperator
	case t.InCategory(chroma.Punctuation):
		return clsPunctuation
	}
	return clsText
}

// highlight returns a class for every rune of every line.
func highlight(lexer chroma.Lexer, lines [][]rune) [][]class {
	out := make([][]class, len(lines))
	for i, l := range lines {
		out[i] = make([]class, len(l))
	}

	var text []rune
	for i, l := range lines {
		if i > 0 {
			text = append(text, '\n')
		}
		text = append(text, l...)
	}
	it, err := lexer.Tokenise(&chroma.TokeniseOptions{State: "root", EnsureLF: true}, string(text))
	if err != nil {
		return out
	}

	row, col := 0, 0
	for tok := it(); tok != chroma.EOF; tok = it() {
		cls := classify(tok.Type)
		for _, r := range tok.Value {
			if r == '\n' {
				row, col = row+1, 0
				continue
			}
			if row < len(out) && col < len(out[row]) {
				out[row][col] = cls
			}
			col++
		}
	}
	return out
}
