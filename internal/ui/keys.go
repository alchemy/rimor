package ui

// The keys of every pane and dialog, for the help overlay (F1). Each pane
// still handles its keys in its own Update; TestKeyTablesMatchHandlers
// reads those handlers and fails when one handles a key its table does not
// list, so the two cannot drift apart.

// binding is one entry of a key table: keys that do the same thing.
type binding struct {
	keys []string // as Bubble Tea names them, shown in this order
	desc string
	// when limits the binding to some states, for the "here" view; nil
	// means always. The "all keys" view shows every binding.
	when func(m *Model) bool
}

// keyScope is the key table of a pane or dialog.
type keyScope struct {
	title string
	keys  []binding
}

var (
	explorerKeys = keyScope{"Explorer", []binding{
		{keys: []string{"up", "k", "down", "j"}, desc: "Move"},
		{keys: []string{"pgup", "ctrl+u", "pgdown", "ctrl+d"}, desc: "Half a page up / down"},
		{keys: []string{"home", "g", "end", "G"}, desc: "First / last row"},
		{keys: []string{"enter", "space"}, desc: "Open; expand or collapse"},
		{keys: []string{"right", "l"}, desc: "Expand, then step inside"},
		{keys: []string{"left", "h"}, desc: "Collapse, or go to the parent"},
		{keys: []string{"a"}, desc: "Add a connection"},
		{keys: []string{"e"}, desc: "Edit the connection"},
		{keys: []string{"d", "delete"}, desc: "Delete the connection"},
		{keys: []string{"r"}, desc: "Refresh what is below the row"},
		{keys: []string{"x"}, desc: "Disconnect"},
		{keys: []string{"/"}, desc: "Filter the loaded rows by name"},
		{keys: []string{"s"}, desc: "Scripts for the object: select, insert, call…", when: func(m *Model) bool {
			return m.explorer.scriptable()
		}},
		{keys: []string{"S"}, desc: "Open the object's first script", when: func(m *Model) bool {
			return m.explorer.scriptable()
		}},
		{keys: []string{"."}, desc: "Show or hide empty schemas", when: func(m *Model) bool {
			return m.explorer.cursor.database() != nil
		}},
	}}

	explorerFilterKeys = keyScope{"Explorer filter", []binding{
		{keys: []string{"up", "ctrl+p", "ctrl+k", "down", "ctrl+n", "ctrl+j"}, desc: "Previous / next match"},
		{keys: []string{"pgup", "pgdown"}, desc: "Half a page of matches up / down"},
		{keys: []string{"enter"}, desc: "Go to the match, opening the path to it"},
		{keys: []string{"esc"}, desc: "Close the filter; the tree is as before"},
		{keys: []string{"backspace"}, desc: "Delete a character"},
		{keys: []string{"ctrl+u"}, desc: "Clear the text"},
	}}

	messagesKeys = keyScope{"Messages", []binding{
		{keys: []string{"up", "k", "down", "j"}, desc: "Scroll"},
		{keys: []string{"pgup", "ctrl+u", "pgdown", "ctrl+d"}, desc: "Page up / down"},
		{keys: []string{"g", "ctrl+home", "G", "ctrl+end"}, desc: "First / last message"},
		{keys: []string{"y"}, desc: "Copy the messages"},
		{keys: []string{"esc"}, desc: "Back to the results"},
	}}

	passwordKeys = keyScope{"Password", []binding{
		{keys: []string{"enter"}, desc: "Connect with it; it is kept in memory only"},
		{keys: []string{"esc"}, desc: "Cancel"},
	}}

	scriptMenuKeys = keyScope{"Scripts", []binding{
		{keys: []string{"up", "k", "down", "j"}, desc: "Move"},
		{keys: []string{"home", "g", "end", "G"}, desc: "First / last script"},
		{keys: []string{"enter"}, desc: "Open the script in a new tab; it does not run"},
		{keys: []string{"esc", "q"}, desc: "Cancel"},
	}}

	resultsKeys = keyScope{"Results", []binding{
		{keys: []string{"up", "k", "down", "j", "left", "h", "right", "l"}, desc: "Move"},
		{keys: []string{"pgup", "ctrl+u", "pgdown", "ctrl+d"}, desc: "Page up / down"},
		{keys: []string{"g", "ctrl+home", "G", "ctrl+end"}, desc: "First / last row"},
		{keys: []string{"home", "0", "end", "$"}, desc: "First / last column"},
		{keys: []string{"enter"}, desc: "Open the cell: view, copy, edit"},
		{keys: []string{"y"}, desc: "Copy the cell"},
		{keys: []string{"Y"}, desc: "Copy the row, tab-separated"},
		{keys: []string{"[", "]"}, desc: "Previous / next result of the batch", when: func(m *Model) bool {
			r := m.currentRun()
			return r != nil && r.res != nil && len(r.res.Sets()) > 1
		}},
		{keys: []string{"m"}, desc: "The batch's messages: PRINT, row counts, errors", when: func(m *Model) bool {
			msgs, _ := runMessages(m.currentRun())
			return len(msgs) > 0
		}},
		{keys: []string{"esc"}, desc: "Stop fetching rows", when: func(m *Model) bool {
			t := m.query.current()
			return t != nil && t.run != nil && t.run.fetching()
		}},
	}}

	tabKeys = keyScope{"Query tabs", []binding{
		{keys: []string{"ctrl+enter", "f5", "alt+enter"}, desc: "Run the selection, or the whole tab"},
		{keys: []string{"esc"}, desc: "Cancel the running query", when: func(m *Model) bool {
			t := m.query.current()
			return t != nil && t.run.busy()
		}},
		{keys: []string{"ctrl+s"}, desc: "Save the tab"},
		{keys: []string{"alt+s"}, desc: "Save as"},
		{keys: []string{"ctrl+w"}, desc: "Close the tab"},
		{keys: []string{"ctrl+tab", "ctrl+pgdown", "alt+]"}, desc: "Next tab"},
		{keys: []string{"ctrl+shift+tab", "ctrl+pgup", "alt+["}, desc: "Previous tab"},
		{keys: []string{"ctrl+e"}, desc: "Change where the tab runs"},
	}}

	editorKeys = keyScope{"Editor", []binding{
		{keys: []string{"up", "down", "left", "right"}, desc: "Move; with shift, select"},
		{keys: []string{"ctrl+left", "ctrl+right", "alt+left", "alt+right"}, desc: "Word left / right"},
		{keys: []string{"home", "end"}, desc: "Line start (first text, then column 1) / end"},
		{keys: []string{"pgup", "pgdown"}, desc: "Page up / down"},
		{keys: []string{"ctrl+home", "ctrl+end"}, desc: "Start / end of the text"},
		{keys: []string{"ctrl+a"}, desc: "Select all"},
		{keys: []string{"ctrl+c", "ctrl+x"}, desc: "Copy / cut (the line, without a selection)"},
		{keys: []string{"ctrl+v"}, desc: "Paste the clipboard, if the terminal shares it; else what rimor copied"},
		{keys: []string{"ctrl+z"}, desc: "Undo"},
		{keys: []string{"ctrl+y", "ctrl+shift+z"}, desc: "Redo"},
		{keys: []string{"enter"}, desc: "New line, keeping the indentation"},
		{keys: []string{"tab", "shift+tab"}, desc: "Indent / dedent the selected lines"},
		{keys: []string{"backspace", "delete"}, desc: "Delete a character"},
		{keys: []string{"ctrl+backspace", "alt+backspace", "ctrl+h"}, desc: "Delete the word before"},
		{keys: []string{"ctrl+delete", "alt+delete", "alt+d"}, desc: "Delete the word after"},
		{keys: []string{"esc"}, desc: "Clear the selection"},
	}}

	paneKeys = keyScope{"Panes", []binding{
		{keys: []string{"tab", "shift+tab"}, desc: "Next / previous pane"},
		{keys: []string{"1", "2", "3"}, desc: "Explorer, query, results"},
		{keys: []string{"?"}, desc: "These keys"},
		{keys: []string{"q", "ctrl+c"}, desc: "Quit"},
	}}

	cellKeys = keyScope{"Cell", []binding{
		{keys: []string{"enter"}, desc: "Save", when: func(m *Model) bool {
			p, _ := m.modal.(*cellPopup)
			return p != nil && p.phase == cellEditing && !p.multiline
		}},
		{keys: []string{"enter"}, desc: "New line", when: func(m *Model) bool {
			p, _ := m.modal.(*cellPopup)
			return p != nil && p.phase == cellEditing && p.multiline
		}},
		{keys: []string{"ctrl+s"}, desc: "Save"},
		{keys: []string{"alt+enter"}, desc: "New line"},
		{keys: []string{"ctrl+f"}, desc: "Format the JSON", when: func(m *Model) bool {
			p, _ := m.modal.(*cellPopup)
			return p != nil && p.json
		}},
		{keys: []string{"ctrl+n"}, desc: "Set NULL"},
		{keys: []string{"esc"}, desc: "Close without saving"},
	}}

	connFormKeys = keyScope{"Connection", []binding{
		{keys: []string{"tab", "down", "shift+tab", "up"}, desc: "Next / previous field"},
		{keys: []string{"left", "h", "right", "l", "space"}, desc: "Change the driver or read-only"},
		{keys: []string{"y", "n"}, desc: "Read-only: yes / no"},
		{keys: []string{"ctrl+t"}, desc: "Test the connection"},
		{keys: []string{"ctrl+s", "enter"}, desc: "Save (enter: on the last field)"},
		{keys: []string{"esc"}, desc: "Cancel"},
	}}

	confirmDeleteKeys = keyScope{"Delete connection", []binding{
		{keys: []string{"y", "enter"}, desc: "Delete"},
		{keys: []string{"n", "esc", "q"}, desc: "Keep"},
	}}

	pathKeys = keyScope{"File", []binding{
		{keys: []string{"tab"}, desc: "Complete the name"},
		{keys: []string{"enter"}, desc: "Open or save"},
		{keys: []string{"esc"}, desc: "Cancel"},
	}}

	closeTabKeys = keyScope{"Close tab", []binding{
		{keys: []string{"s", "enter"}, desc: "Save, then close"},
		{keys: []string{"d"}, desc: "Close without saving"},
		{keys: []string{"esc", "n", "q"}, desc: "Keep the tab"},
	}}

	pickerKeys = keyScope{"Query context", []binding{
		{keys: []string{"up", "ctrl+p", "ctrl+k", "down", "ctrl+n", "ctrl+j"}, desc: "Move"},
		{keys: []string{"pgup", "pgdown", "home", "end"}, desc: "Page up / down, first / last"},
		{keys: []string{"right", "tab"}, desc: "List the connection's databases"},
		{keys: []string{"enter"}, desc: "Choose"},
		{keys: []string{"esc"}, desc: "Cancel"},
	}}

	helpKeys = keyScope{"Keys", []binding{
		{keys: []string{"tab"}, desc: "This place's keys / all keys"},
		{keys: []string{"up", "down", "pgup", "pgdown"}, desc: "Scroll"},
		{keys: []string{"backspace"}, desc: "Delete from the filter"},
		{keys: []string{"esc"}, desc: "Clear the filter, then close"},
	}}
)

// allScopes is every key table, in the order the "all keys" view lists
// them after the app-wide keys.
var allScopes = []keyScope{
	paneKeys, explorerKeys, explorerFilterKeys, tabKeys, editorKeys, resultsKeys, messagesKeys, cellKeys,
	connFormKeys, passwordKeys, pickerKeys, scriptMenuKeys, pathKeys, closeTabKeys, confirmDeleteKeys, helpKeys,
}

// hereScopes are the key tables that apply where the user is, and a name
// for that place.
func (m *Model) hereScopes() (string, []keyScope) {
	switch md := m.modal.(type) {
	case *cellPopup:
		if md.phase == cellEditing {
			return "Cell", []keyScope{cellKeys, editorKeys}
		}
		return "Cell", []keyScope{cellKeys}
	case *connForm:
		return "Connection", []keyScope{connFormKeys}
	case confirmDelete:
		return "Delete connection", []keyScope{confirmDeleteKeys}
	case *pathPrompt:
		return "File", []keyScope{pathKeys}
	case confirmCloseTab:
		return "Close tab", []keyScope{closeTabKeys}
	case *contextPicker:
		return "Query context", []keyScope{pickerKeys}
	case *scriptMenu:
		return "Scripts", []keyScope{scriptMenuKeys}
	case *passwordPrompt:
		return "Password", []keyScope{passwordKeys}
	}
	if m.focus == focusExplorer && m.explorer.filtering {
		return "Explorer filter", []keyScope{explorerFilterKeys}
	}
	switch m.focus {
	case focusQuery:
		return "Query", []keyScope{tabKeys, editorKeys}
	case focusResults:
		if r := m.currentRun(); r != nil && r.messages {
			return "Messages", []keyScope{messagesKeys, resultsKeys, paneKeys}
		}
		return "Results", []keyScope{resultsKeys, paneKeys}
	}
	return "Explorer", []keyScope{explorerKeys, paneKeys}
}
