package ui

import (
	"context"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

const loadTimeout = 20 * time.Second

// connection is a saved connection and its open database handles. The
// pointer is stable for the life of the entry, so query tabs can hold it
// across edits.
type connection struct {
	pool *db.Pool
}

func newConnection(cfg db.Config) *connection {
	return &connection{pool: db.NewPool(cfg)}
}

func (c *connection) cfg() db.Config { return c.pool.Config() }

func (c *connection) close() { c.pool.Close() }

// setConfig replaces the connection settings, dropping open handles.
func (c *connection) setConfig(cfg db.Config) {
	c.pool.Close()
	c.pool = db.NewPool(cfg)
}

// node is one row of the tree. Children load lazily on first expansion.
type node struct {
	obj    db.Object
	conn   *connection
	parent *node
	root   bool

	children []*node
	expanded bool
	loaded   bool
	loading  bool
	err      error
	// gen invalidates in-flight loads when the node is reset or reloaded.
	gen int
	// showAll lists the empty schemas of a database, hidden by default.
	showAll bool
}

func (n *node) expandable() bool { return n.root || n.obj.Expandable() }

// visibleChildren are the children the tree shows: all of them, except
// empty schemas unless the database shows all. hidden counts the rest.
func (n *node) visibleChildren() (visible []*node, hidden int) {
	for _, c := range n.children {
		if c.obj.Kind == db.KindSchema && c.obj.Empty && !n.showAll {
			hidden++
			continue
		}
		visible = append(visible, c)
	}
	return visible, hidden
}

// database is the database node a node belongs to, if any.
func (n *node) database() *node {
	for ; n != nil; n = n.parent {
		if !n.root && n.obj.Kind == db.KindDatabase {
			return n
		}
	}
	return nil
}

// reset forgets everything loaded below the node.
func (n *node) reset() {
	n.gen++
	n.children = nil
	n.expanded, n.loaded, n.loading = false, false, false
	n.err = nil
}

// key identifies a node among its siblings across reloads.
func (n *node) key() string {
	return strconv.Itoa(int(n.obj.Kind)) + "\x00" + n.obj.Name + "\x00" + n.obj.Detail
}

type loadedMsg struct {
	node    *node
	gen     int
	objects []db.Object
	err     error
}

// openFormMsg asks for the connection form; a nil conn means a new one.
type openFormMsg struct{ conn *connection }

// disconnectMsg asks to close a connection's open handles.
type disconnectMsg struct{ conn *connection }

// confirmDeleteMsg asks to confirm removing a saved connection.
type confirmDeleteMsg struct{ conn *connection }

// line is one rendered row: a tree node, the "New connection" action, or
// non-selectable text such as an error.
type line struct {
	node  *node
	add   bool
	text  string
	depth int
}

func (l line) selectable() bool { return l.add || l.node != nil }

// Explorer is the connection and catalog tree.
type Explorer struct {
	store  db.Store
	roots  []*node
	cursor *node // nil selects the "New connection" row
	offset int

	width, height int

	spinner spinner.Model
	pending int    // loads in flight; the spinner ticks while > 0
	notice  string // last persistence error
}

func NewExplorer(store db.Store) Explorer {
	e := Explorer{
		store:   store,
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle())),
	}
	cfgs, err := store.Load()
	if err != nil {
		e.notice = "Could not load connections: " + err.Error()
	}
	for _, cfg := range cfgs {
		e.roots = append(e.roots, &node{conn: newConnection(cfg), root: true})
	}
	return e
}

func (e *Explorer) SetSize(width, height int) {
	e.width, e.height = width, height
	e.scroll()
}

// Close releases every open connection.
func (e *Explorer) Close() {
	for _, r := range e.roots {
		r.conn.close()
	}
}

func (e *Explorer) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case loadedMsg:
		e.pending--
		e.applyLoad(msg)
	case spinner.TickMsg:
		if e.pending > 0 {
			var cmd tea.Cmd
			e.spinner, cmd = e.spinner.Update(msg)
			return cmd
		}
	case tea.KeyPressMsg:
		cmd := e.handleKey(msg)
		e.scroll()
		return cmd
	}
	return nil
}

func (e *Explorer) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	page := max(e.height/2, 1)
	switch msg.String() {
	case "up", "k":
		e.move(-1)
	case "down", "j":
		e.move(1)
	case "pgup", "ctrl+u":
		e.move(-page)
	case "pgdown", "ctrl+d":
		e.move(page)
	case "home", "g":
		e.move(-1 << 30)
	case "end", "G":
		e.move(1 << 30)

	case "enter", "space":
		if e.cursor == nil {
			return openForm(nil)
		}
		return e.toggle(e.cursor)
	case "right", "l":
		n := e.cursor
		switch {
		case n == nil:
		case n.expanded && len(n.children) > 0:
			if visible, _ := n.visibleChildren(); len(visible) > 0 {
				e.cursor = visible[0]
			}
		case !n.expanded:
			return e.toggle(n)
		}
	case "left", "h":
		n := e.cursor
		switch {
		case n == nil:
		case n.expanded:
			e.toggle(n)
		case n.parent != nil:
			e.cursor = n.parent
		}

	case "a":
		return openForm(nil)
	case "e":
		if e.cursor != nil {
			return openForm(e.cursor.conn)
		}
	case "d", "delete":
		if e.cursor != nil {
			conn := e.cursor.conn
			return func() tea.Msg { return confirmDeleteMsg{conn} }
		}
	case "r":
		return e.refresh()
	case "x":
		if e.cursor != nil {
			// The model stops the connection's running queries first.
			conn := e.cursor.conn
			return func() tea.Msg { return disconnectMsg{conn} }
		}
	case ".":
		// Show or hide the empty schemas of the database under the cursor.
		if d := e.cursor.database(); d != nil {
			d.showAll = !d.showAll
		}
	}
	return nil
}

func openForm(conn *connection) tea.Cmd {
	return func() tea.Msg { return openFormMsg{conn} }
}

// Context is the connection and database of the selected row: the place a
// new query tab should run.
func (e *Explorer) Context() queryContext {
	n := e.cursor
	if n == nil {
		return queryContext{}
	}
	ctx := queryContext{conn: n.conn}
	for ; n != nil; n = n.parent {
		if !n.root && n.obj.Kind == db.KindDatabase {
			ctx.database = n.obj.Name
			break
		}
	}
	return ctx
}

// Databases returns the loaded databases of a connection, for the context
// picker; empty until the connection has been opened.
func (e *Explorer) Databases(conn *connection) []db.Object {
	r := e.rootOf(conn)
	if r == nil || !r.loaded {
		return nil
	}
	var out []db.Object
	for _, c := range r.children {
		if c.obj.Kind == db.KindDatabase {
			out = append(out, c.obj)
		}
	}
	return out
}

// Connections returns the saved connections in display order.
func (e *Explorer) Connections() []*connection {
	out := make([]*connection, len(e.roots))
	for i, r := range e.roots {
		out[i] = r.conn
	}
	return out
}

// Expand opens a connection in the tree, as the context picker does when
// it needs the connection's databases.
func (e *Explorer) Expand(conn *connection) tea.Cmd {
	if r := e.rootOf(conn); r != nil && !r.loaded && !r.loading {
		cmd := e.load(r)
		e.scroll()
		return cmd
	}
	return nil
}

// ConnectionByName finds a saved connection, for restoring sessions.
func (e *Explorer) ConnectionByName(name string) *connection {
	for _, r := range e.roots {
		if r.conn.cfg().Name == name {
			return r.conn
		}
	}
	return nil
}

// Hints describes the keys that apply to the selected row.
func (e *Explorer) Hints() string {
	switch {
	case e.cursor == nil:
		return hints("⏎", "new connection")
	case e.cursor.root:
		return hints("⏎", "open", "e", "edit", "d", "del")
	case e.cursor.obj.Kind == db.KindDatabase:
		return hints("⏎", "expand", ".", "schemas")
	default:
		return hints("⏎", "expand", "^t", "query")
	}
}

// toggle expands or collapses a node, loading its children the first time.
func (e *Explorer) toggle(n *node) tea.Cmd {
	if !n.expandable() {
		return nil
	}
	if n.expanded {
		n.expanded = false
		if !n.loaded {
			n.err = nil // collapsing clears a failed load; expanding retries
		}
		return nil
	}
	if n.loaded || n.loading {
		n.expanded = true
		return nil
	}
	return e.load(n)
}

// refresh reloads the selected node, or the node holding the selected leaf.
func (e *Explorer) refresh() tea.Cmd {
	n := e.cursor
	for n != nil && !n.expandable() {
		n = n.parent
	}
	if n == nil {
		return nil
	}
	return e.load(n)
}

func (e *Explorer) load(n *node) tea.Cmd {
	n.gen++
	n.loading, n.expanded, n.err = true, true, nil

	gen, pool := n.gen, n.conn.pool
	var parent *db.Object
	if !n.root {
		obj := n.obj
		parent = &obj
	}

	load := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		objs, err := pool.Children(ctx, parent)
		return loadedMsg{node: n, gen: gen, objects: objs, err: err}
	}

	e.pending++
	if e.pending == 1 {
		return tea.Batch(load, e.spinner.Tick)
	}
	return load
}

func (e *Explorer) applyLoad(msg loadedMsg) {
	n := msg.node
	if msg.gen != n.gen {
		return
	}

	n.loading = false
	if msg.err != nil {
		n.err = msg.err
		n.loaded = false
		n.children = nil
		e.scroll()
		return
	}

	// Reuse nodes that survived a reload so their expansion is kept.
	old := make(map[string]*node, len(n.children))
	for _, c := range n.children {
		old[c.key()] = c
	}
	children := make([]*node, 0, len(msg.objects))
	for _, obj := range msg.objects {
		c := &node{obj: obj, conn: n.conn, parent: n}
		if prev, ok := old[c.key()]; ok {
			c = prev
		}
		children = append(children, c)
	}
	n.children = children
	n.loaded = true
	e.scroll()
}

func (e *Explorer) disconnect(conn *connection) {
	conn.close()
	if r := e.rootOf(conn); r != nil {
		r.reset()
	}
	e.scroll()
}

// Upsert saves a new connection (conn == nil) or replaces an existing one.
func (e *Explorer) Upsert(conn *connection, cfg db.Config) {
	if conn == nil {
		r := &node{conn: newConnection(cfg), root: true}
		e.roots = append(e.roots, r)
		e.cursor = r
	} else if r := e.rootOf(conn); r != nil {
		conn.setConfig(cfg)
		r.reset()
		e.cursor = r
	}
	e.save()
	e.scroll()
}

// Remove deletes a saved connection and selects its neighbour.
func (e *Explorer) Remove(conn *connection) {
	for i, r := range e.roots {
		if r.conn != conn {
			continue
		}
		conn.close()
		r.reset()
		e.roots = append(e.roots[:i], e.roots[i+1:]...)
		switch {
		case i < len(e.roots):
			e.cursor = e.roots[i]
		case i > 0:
			e.cursor = e.roots[i-1]
		default:
			e.cursor = nil
		}
		break
	}
	e.save()
	e.scroll()
}

// Names returns the saved connection names, for uniqueness checks.
func (e *Explorer) Names() []string {
	names := make([]string, len(e.roots))
	for i, r := range e.roots {
		names[i] = r.conn.cfg().Name
	}
	return names
}

func (e *Explorer) save() {
	cfgs := make([]db.Config, len(e.roots))
	for i, r := range e.roots {
		cfgs[i] = r.conn.cfg()
	}
	e.notice = ""
	if err := e.store.Save(cfgs); err != nil {
		e.notice = "Could not save connections: " + err.Error()
	}
}

func (e *Explorer) rootOf(conn *connection) *node {
	for _, r := range e.roots {
		if r.conn == conn {
			return r
		}
	}
	return nil
}

// lines flattens the visible tree.
func (e *Explorer) lines() []line {
	ls := []line{{add: true}, {}}
	if len(e.roots) == 0 {
		ls = append(ls, line{text: mutedStyle.Render("No saved connections yet.")})
	}

	var walk func(n *node, depth int)
	walk = func(n *node, depth int) {
		ls = append(ls, line{node: n, depth: depth})
		if !n.expanded {
			return
		}
		if n.err != nil {
			for _, t := range wrapLines(n.err.Error(), e.width-3-2*(depth+1), 4) {
				ls = append(ls, line{text: errorStyle.Render(t), depth: depth + 1})
			}
			return
		}
		visible, hidden := n.visibleChildren()
		if n.loaded && len(n.children) == 0 {
			ls = append(ls, line{text: mutedStyle.Italic(true).Render("empty"), depth: depth + 1})
		}
		for _, c := range visible {
			walk(c, depth+1)
		}
		// Say what the filter hides: "nothing visible" also covers schemas
		// whose objects the login may not see.
		if hidden > 0 {
			what := strconv.Itoa(hidden) + " schemas"
			if hidden == 1 {
				what = "1 schema"
			}
			note := what + " with nothing visible · . shows"
			for _, t := range wrapLines(note, e.width-3-2*(depth+1), 2) {
				ls = append(ls, line{text: mutedStyle.Italic(true).Render(t), depth: depth + 1})
			}
		}
	}
	for _, r := range e.roots {
		walk(r, 0)
	}

	if e.notice != "" {
		ls = append(ls, line{})
		for _, t := range wrapLines(e.notice, e.width-2, 4) {
			ls = append(ls, line{text: errorStyle.Render(t)})
		}
	}
	return ls
}

// wrapLines word-wraps s to width, keeping at most limit lines.
func wrapLines(s string, width, limit int) []string {
	out := strings.Split(ansi.Wrap(s, max(width, 8), ""), "\n")
	if len(out) > limit {
		out = out[:limit]
		out[limit-1] = ansi.Truncate(out[limit-1], max(width, 8)-1, "") + "…"
	}
	return out
}

func (e *Explorer) indexOf(ls []line, n *node) int {
	for i, l := range ls {
		if (n == nil && l.add) || (n != nil && l.node == n) {
			return i
		}
	}
	return -1
}

// cursorIndex finds the selected line, moving the cursor up to the nearest
// visible ancestor when its node was collapsed away or removed.
func (e *Explorer) cursorIndex(ls []line) int {
	for {
		if i := e.indexOf(ls, e.cursor); i >= 0 {
			return i
		}
		e.cursor = e.cursor.parent
	}
}

func (e *Explorer) move(delta int) {
	ls := e.lines()
	cur := e.cursorIndex(ls)
	var sel []int
	pos := 0
	for i, l := range ls {
		if l.selectable() {
			if i == cur {
				pos = len(sel)
			}
			sel = append(sel, i)
		}
	}
	pos = min(max(pos+delta, 0), len(sel)-1)
	l := ls[sel[pos]]
	if l.add {
		e.cursor = nil
	} else {
		e.cursor = l.node
	}
}

// scroll keeps the cursor inside the viewport.
func (e *Explorer) scroll() {
	if e.height <= 0 {
		return
	}
	ls := e.lines()
	cur := e.cursorIndex(ls)
	if cur < e.offset {
		e.offset = cur
	}
	if cur >= e.offset+e.height {
		e.offset = cur - e.height + 1
	}
	e.offset = min(e.offset, max(len(ls)-e.height, 0))
	if cur <= 1 {
		e.offset = 0 // keep the "New connection" row in view at the top
	}
}

func (e *Explorer) View(focused bool) string {
	ls := e.lines()
	cur := e.cursorIndex(ls)
	end := min(e.offset+e.height, len(ls))

	rows := make([]string, 0, end-e.offset)
	for i := e.offset; i < end; i++ {
		rows = append(rows, e.renderLine(ls[i], i == cur, focused))
	}
	return strings.Join(rows, "\n")
}

func (e *Explorer) renderLine(l line, current, focused bool) string {
	selected := current && focused
	st := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return onCursor(s)
		}
		return s
	}
	plain := lipgloss.NewStyle()
	gap := st(plain).Render(" ")
	indent := st(plain).Render(" " + strings.Repeat("  ", l.depth))

	var s string
	switch {
	case l.add:
		label := accentStyle
		if selected {
			label = label.Bold(true)
		}
		s = indent + st(plain).Render("  ") + st(accentStyle).Render(iconAdd.String()) + gap +
			st(label).Render("New connection")

	case l.node != nil:
		n := l.node

		chevron := st(plain).Render(" ")
		switch {
		case n.loading:
			chevron = st(accentStyle).Render(e.spinner.View())
		case n.err != nil:
			chevron = st(errorStyle).Render("✗")
		case n.expandable() && n.expanded:
			chevron = st(mutedStyle).Render("▾")
		case n.expandable():
			chevron = st(mutedStyle).Render("▸")
		}

		var ic icon
		name, detail := n.obj.Name, n.obj.Detail
		nameStyle := textStyle
		if n.root {
			cfg := n.conn.cfg()
			ic = driverIcon(cfg.Driver)
			if !n.conn.pool.Connected() {
				ic.color = &colorMuted // dim until connected
			}
			name, detail = cfg.Name, cfg.Driver.Short()
			if cfg.ReadOnly {
				detail += " · read-only"
			}
			nameStyle = nameStyle.Bold(true)
		} else {
			ic = objectIcon(n.obj, n.expanded)
			switch {
			case n.obj.Kind == db.KindFolder:
				nameStyle = titleStyle
				if n.loaded {
					detail = strconv.Itoa(len(n.children))
				}
			case n.obj.Kind == db.KindSchema && n.obj.Empty:
				nameStyle, detail = mutedStyle, "empty" // shown only with all schemas
			case n.obj.Kind == db.KindDatabase && n.showAll:
				detail = strings.TrimSpace(detail + "  + empty") // empty schemas are listed
			}
		}
		if current && !focused {
			nameStyle = nameStyle.Foreground(colorAccent)
		}

		s = indent + chevron + gap + st(iconStyle(ic)).Render(ic.String()) + gap +
			st(nameStyle).Render(name)
		if detail != "" {
			s += st(plain).Render("  ") + st(mutedStyle).Render(detail)
		}

	default:
		s = indent + "  " + l.text
	}

	s = ansi.Truncate(s, e.width, "…")
	if pad := e.width - lipgloss.Width(s); pad > 0 {
		s += st(plain).Render(strings.Repeat(" ", pad))
	}
	return s
}
