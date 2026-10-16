package ui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// The explorer's filter (/): as the user types, the tree shrinks to the
// loaded rows whose names contain the text, with the path to each, opened
// for the filter only. It covers what the explorer has loaded; the rest of
// a catalog is not known until its level is opened.

// startFilter opens the filter line.
func (e *Explorer) startFilter() {
	e.filtering, e.filter, e.beforeFilter = true, "", e.cursor
	e.offset = 0
}

// endFilter closes the filter. Accepting keeps the cursor on the chosen
// match and opens the path to it; otherwise the tree is as it was.
func (e *Explorer) endFilter(accept bool) {
	ls := e.lines()
	chosen := e.cursorIndex(ls) >= 0 && e.filter != ""
	e.filtering, e.filter = false, ""
	if accept && chosen {
		for p := e.cursor.parent; p != nil; p = p.parent {
			p.expanded = true
		}
		if d := e.cursor.database(); d != nil && e.cursor.obj.Kind == db.KindSchema && e.cursor.obj.Empty {
			d.showAll = true // the match was an empty schema, hidden otherwise
		}
	} else {
		e.cursor = e.beforeFilter
	}
	e.beforeFilter = nil
	e.scroll()
}

// handleFilterKey edits the filter and moves between its matches.
func (e *Explorer) handleFilterKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		e.endFilter(false)
	case "enter":
		e.endFilter(true)
	case "up", "ctrl+p", "ctrl+k":
		e.move(-1)
	case "down", "ctrl+n", "ctrl+j":
		e.move(1)
	case "pgup":
		e.move(-max(e.listHeight()/2, 1))
	case "pgdown":
		e.move(max(e.listHeight()/2, 1))
	case "backspace":
		if r := []rune(e.filter); len(r) > 0 {
			e.setFilter(string(r[:len(r)-1]))
		}
	case "ctrl+u":
		e.setFilter("")
	default:
		if msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			e.setFilter(e.filter + msg.Text)
		}
	}
	return nil
}

// setFilter changes the text and keeps the cursor on a match: the same
// one if it still matches, else the first.
func (e *Explorer) setFilter(text string) {
	e.filter = text
	ls := e.lines()
	if i := e.indexOf(ls, e.cursor); i >= 0 && ls[i].match {
		e.scroll()
		return
	}
	e.cursor = nil
	for _, l := range ls {
		if l.match {
			e.cursor = l.node
			break
		}
	}
	e.offset = 0
	e.scroll()
}

// matches reports whether a node's name contains the filter, ignoring case.
func (e *Explorer) matches(n *node) bool {
	return strings.Contains(strings.ToLower(nodeName(n)), strings.ToLower(e.filter))
}

// nodeName is the name a row shows: the connection's for a root.
func nodeName(n *node) string {
	if n.root {
		return n.conn.cfg().Name
	}
	return n.obj.Name
}

// filterLines lists the loaded rows that match, each under the rows of its
// path, which are there for context and cannot be selected. Empty schemas
// a database hides stay hidden and are counted instead.
func (e *Explorer) filterLines() (ls []line, matches int) {
	if e.filter == "" {
		return nil, 0
	}
	hiddenMatches := 0
	// walk adds n and the matching rows below it, returning whether any
	// row was added.
	var walk func(n *node, depth int) bool
	walk = func(n *node, depth int) bool {
		at := len(ls)
		self := e.matches(n)
		ls = append(ls, line{node: n, depth: depth, match: self, context: !self})
		found := self
		if n.loaded {
			for _, c := range n.children {
				if c.obj.Kind == db.KindSchema && c.obj.Empty && !n.showAll {
					if e.matches(c) {
						hiddenMatches++
					}
					continue
				}
				if walk(c, depth+1) {
					found = true
				}
			}
		}
		if !found {
			ls = ls[:at]
		} else if len(ls) > at+1 {
			ls[at].opened = true // its matching rows are below it
		}
		return found
	}
	for _, r := range e.roots {
		walk(r, 0)
	}
	for _, l := range ls {
		if l.match {
			matches++
		}
	}
	if hiddenMatches > 0 {
		what := strconv.Itoa(hiddenMatches) + " empty schemas"
		if hiddenMatches == 1 {
			what = "1 empty schema"
		}
		ls = append(ls, line{}, line{text: mutedStyle.Italic(true).Render(what + " also match, hidden")})
	}
	return ls, matches
}

// filterBar is the filter's input line, above the tree.
func (e *Explorer) filterBar(matches int) string {
	bar := accentStyle.Render(" / ") + textStyle.Render(e.filter) + accentStyle.Render("▏")
	switch {
	case e.filter == "":
		bar += mutedStyle.Italic(true).Render("type to filter what is loaded")
	case matches == 0:
		bar += mutedStyle.Render("  no match")
	case matches == 1:
		bar += mutedStyle.Render("  1 match")
	default:
		bar += mutedStyle.Render("  " + strconv.Itoa(matches) + " matches")
	}
	return bar
}

// highlight renders name with the part matching the filter emphasised.
// Names whose case folding changes their length are left plain.
func (e *Explorer) highlight(name string, base lipgloss.Style, st func(lipgloss.Style) lipgloss.Style) string {
	lower := strings.ToLower(name)
	i := strings.Index(lower, strings.ToLower(e.filter))
	if e.filter == "" || i < 0 || len(lower) != len(name) {
		return st(base).Render(name)
	}
	j := i + len(strings.ToLower(e.filter))
	mark := base.Foreground(colorAccent).Bold(true).Underline(true)
	return st(base).Render(name[:i]) + st(mark).Render(name[i:j]) + st(base).Render(name[j:])
}
