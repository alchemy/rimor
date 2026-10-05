package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// Tool calls from an agent (see package agent). Calls that only look at
// the UI are answered in Update; those that query the catalog run as
// commands and answer from there. None runs SQL the agent wrote.

// AgentRequestMsg is a tool call; the answer goes to Reply, once.
type AgentRequestMsg struct {
	Tool  string
	Args  json.RawMessage
	Reply chan<- AgentReply // buffered
}

type AgentReply struct {
	Result any
	Err    error
}

// agentArgs are the arguments of every tool; each uses some.
type agentArgs struct {
	Connection string `json:"connection"`
	Database   string `json:"database"`
	Schema     string `json:"schema"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Text       string `json:"text"`
	SQL        string `json:"sql"`
	Title      string `json:"title"`
	Tab        *int   `json:"tab"`
}

const (
	searchLimit  = 200
	listPerKind  = 5000 // objects of one kind list_objects returns
	versionWait  = 3 * time.Second
	agentTabName = "agent"
)

func (m *Model) agentRequest(msg AgentRequestMsg) tea.Cmd {
	reply := func(result any, err error) { msg.Reply <- AgentReply{result, err} }
	var a agentArgs
	if len(msg.Args) > 0 && string(msg.Args) != "null" {
		if err := json.Unmarshal(msg.Args, &a); err != nil {
			reply(nil, fmt.Errorf("bad arguments: %w", err))
			return nil
		}
	}

	switch msg.Tool {
	case "list_connections":
		return m.agentConnections(reply)
	case "list_tabs":
		reply(m.agentTabs(), nil)
		return nil
	case "read_tab":
		reply(m.agentReadTab(a))
		return nil
	case "open_query":
		reply(m.agentOpen(a))
		return nil
	case "update_query":
		reply(m.agentUpdate(a))
		return nil
	}

	// Catalog tools: resolve the connection here, query in the background.
	conn, err := m.agentConnection(a.Connection)
	if err != nil {
		reply(nil, err)
		return nil
	}
	pool := conn.pool
	var work func(ctx context.Context) (any, error)
	switch msg.Tool {
	case "list_objects":
		work = func(ctx context.Context) (any, error) { return listObjects(ctx, pool, a) }
	case "search_objects":
		if strings.TrimSpace(a.Text) == "" {
			reply(nil, errors.New("text is required"))
			return nil
		}
		work = func(ctx context.Context) (any, error) {
			matches, err := pool.Search(ctx, agentDatabase(pool, a.Database), strings.TrimSpace(a.Text), searchLimit+1)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"matches": matches}
			if len(matches) > searchLimit {
				out["matches"], out["truncated"] = matches[:searchLimit], true
			}
			return out, nil
		}
	case "describe":
		if a.Name == "" {
			reply(nil, errors.New("name is required"))
			return nil
		}
		work = func(ctx context.Context) (any, error) {
			schema := a.Schema
			if !pool.Config().Driver.HasDatabases() {
				schema = ""
			}
			return pool.Describe(ctx, agentDatabase(pool, a.Database), schema, a.Name)
		}
	default:
		reply(nil, fmt.Errorf("unknown tool %q", msg.Tool))
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		reply(work(ctx))
		return nil
	}
}

// WithAgentError tells the user that agents cannot reach this rimor.
func (m Model) WithAgentError(err error) Model {
	m.query.notice = errorStyle.Render("Agents cannot reach rimor: " + err.Error())
	return m
}

// agentDatabase ignores the database for engines that have none.
func agentDatabase(pool *db.Pool, database string) string {
	if !pool.Config().Driver.HasDatabases() {
		return ""
	}
	return database
}

// agentConnection finds a saved connection by name, ignoring case when
// that is unambiguous.
func (m *Model) agentConnection(name string) (*connection, error) {
	if name == "" {
		return nil, errors.New("connection is required: see list_connections")
	}
	conns := m.explorer.Connections()
	var folded []*connection
	for _, c := range conns {
		switch {
		case c.cfg().Name == name:
			return c, nil
		case strings.EqualFold(c.cfg().Name, name):
			folded = append(folded, c)
		}
	}
	if len(folded) == 1 {
		return folded[0], nil
	}
	names := make([]string, len(conns))
	for i, c := range conns {
		names[i] = fmt.Sprintf("%q", c.cfg().Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no connection named %q: the user has no saved connections", name)
	}
	return nil, fmt.Errorf("no connection named %q; the connections are %s", name, strings.Join(names, ", "))
}

type agentConn struct {
	Name          string `json:"name"`
	Driver        string `json:"driver"`
	ReadOnly      bool   `json:"read_only"`
	Open          bool   `json:"open"`
	ServerVersion string `json:"server_version,omitempty"`
}

// agentConnections lists the connections, with the version of those
// already open; it never opens one.
func (m *Model) agentConnections(reply func(any, error)) tea.Cmd {
	conns := m.explorer.Connections()
	pools := make([]*db.Pool, len(conns))
	for i, c := range conns {
		pools[i] = c.pool
	}
	return func() tea.Msg {
		out := make([]agentConn, len(pools))
		done := make(chan struct{}, len(pools))
		for i, p := range pools {
			cfg := p.Config()
			out[i] = agentConn{Name: cfg.Name, Driver: cfg.Driver.Label(), ReadOnly: cfg.ReadOnly, Open: p.Connected()}
			if !out[i].Open {
				done <- struct{}{}
				continue
			}
			go func() {
				defer func() { done <- struct{}{} }()
				ctx, cancel := context.WithTimeout(context.Background(), versionWait)
				defer cancel()
				if v, err := p.ServerVersion(ctx); err == nil {
					out[i].ServerVersion = v
				}
			}()
		}
		for range pools {
			<-done
		}
		reply(map[string]any{"connections": out}, nil)
		return nil
	}
}

// listObjects walks one level of the catalog; see the list_objects tool.
func listObjects(ctx context.Context, pool *db.Pool, a agentArgs) (any, error) {
	if !pool.Config().Driver.HasDatabases() {
		folders, err := pool.Children(ctx, nil)
		if err != nil {
			return nil, err
		}
		return objectsByKind(ctx, pool, folders, a.Kind)
	}

	if a.Database == "" && a.Schema == "" {
		dbs, err := pool.Children(ctx, nil)
		if err != nil {
			return nil, err
		}
		type database struct {
			Name    string `json:"name"`
			Default bool   `json:"default,omitempty"`
		}
		out := make([]database, len(dbs))
		for i, d := range dbs {
			out[i] = database{d.Name, d.Detail == "default"}
		}
		version, _ := pool.ServerVersion(ctx)
		return map[string]any{"server_version": version, "databases": out}, nil
	}

	if a.Schema == "" {
		parent := db.Object{Kind: db.KindDatabase, Name: a.Database, Database: a.Database}
		schemas, err := pool.Children(ctx, &parent)
		if err != nil {
			return nil, err
		}
		names, empty := []string{}, []string{}
		for _, s := range schemas {
			if s.Empty {
				empty = append(empty, s.Name)
			} else {
				names = append(names, s.Name)
			}
		}
		out := map[string]any{"schemas": names}
		if len(empty) > 0 {
			out["empty_schemas"] = empty
		}
		return out, nil
	}

	parent := db.Object{Kind: db.KindSchema, Name: a.Schema, Schema: a.Schema, Database: a.Database}
	folders, err := pool.Children(ctx, &parent)
	if err != nil {
		return nil, err
	}
	return objectsByKind(ctx, pool, folders, a.Kind)
}

// objectsByKind lists the content of folders, by kind; only of one kind
// when kind is set.
func objectsByKind(ctx context.Context, pool *db.Pool, folders []db.Object, kind string) (any, error) {
	out := map[string]any{}
	known := false
	for _, f := range folders {
		if f.Kind != db.KindFolder {
			continue
		}
		name := db.KindName(f.Contains)
		if kind != "" && name != kind {
			continue
		}
		known = true
		objs, err := pool.Children(ctx, &f)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(objs))
		for _, o := range objs {
			switch {
			case o.Kind == db.KindFunction && o.Detail != "":
				names = append(names, o.Name+o.Detail) // the arguments
			case (o.Kind == db.KindIndex || o.Kind == db.KindTrigger) && o.Detail != "":
				names = append(names, o.Name+" on "+o.Detail)
			default:
				names = append(names, o.Name)
			}
		}
		key := strings.ReplaceAll(name, " ", "_") + "s"
		if len(names) > listPerKind {
			names = names[:listPerKind]
			out[key+"_truncated"] = true
		}
		out[key] = names
	}
	if kind != "" && !known {
		return nil, fmt.Errorf("no %ss here", kind)
	}
	return out, nil
}

type agentTab struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Connection string `json:"connection,omitempty"`
	Database   string `json:"database,omitempty"`
	ByAgent    bool   `json:"opened_by_agent,omitempty"`
	Current    bool   `json:"current,omitempty"`
	Unsaved    bool   `json:"unsaved,omitempty"`
}

func (m *Model) describeTab(t *tab) agentTab {
	a := agentTab{ID: t.id, Title: t.title(), Database: t.ctx.database, ByAgent: t.agent,
		Current: t == m.query.current(), Unsaved: t.dirty()}
	if t.ctx.conn != nil {
		a.Connection = t.ctx.conn.cfg().Name
	}
	return a
}

func (m *Model) agentTabs() any {
	out := make([]agentTab, len(m.query.tabs))
	for i, t := range m.query.tabs {
		out[i] = m.describeTab(t)
	}
	return map[string]any{"tabs": out}
}

func (m *Model) agentTab(id *int) (*tab, error) {
	if id == nil {
		if t := m.query.current(); t != nil {
			return t, nil
		}
		return nil, errors.New("no query tab is open")
	}
	for _, t := range m.query.tabs {
		if t.id == *id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no tab %d: it was closed, or rimor restarted; see list_tabs", *id)
}

func (m *Model) agentReadTab(a agentArgs) (any, error) {
	t, err := m.agentTab(a.Tab)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"tab": m.describeTab(t), "sql": t.ed.Text()}
	if sel := t.ed.SelectedText(); sel != "" {
		out["selection"] = sel
	}
	if t.run != nil {
		out["last_run"] = runOutcome(t.run)
	}
	return out, nil
}

// runOutcome is what an agent learns of a run: how it ended, never the
// rows. A PostgreSQL error's detail is left out, since it may quote them
// ("Key (email)=(…) already exists").
func runOutcome(r *run) map[string]any {
	out := map[string]any{"statement": r.query}
	if r.line > 0 {
		out["statement_line"] = r.line + 1 // where a selection started
	}
	switch {
	case r.running:
		out["status"] = "running"
		return out
	case r.err != nil:
		out["status"] = "error"
		e := map[string]any{"message": r.err.Message}
		if r.err.Code != "" {
			e["code"] = r.err.Code
		}
		if r.err.Hint != "" {
			e["hint"] = r.err.Hint
		}
		if r.err.Line > 0 {
			e["line"] = r.err.Line
		}
		if r.err.Position > 0 {
			e["position"] = r.err.Position
		}
		out["error"] = e
		return out
	case r.res == nil:
		out["status"] = "done"
		return out
	}

	res := r.res
	out["duration_ms"] = res.Duration.Milliseconds()
	if !res.HasRows() {
		out["status"] = "done"
		if res.HasAffected {
			out["rows_affected"] = res.RowsAffected
		}
		return out
	}
	type column struct {
		Name string `json:"name"`
		Type string `json:"type,omitempty"`
	}
	cols := res.Rows.Columns()
	columns := make([]column, len(cols))
	for i, c := range cols {
		columns[i] = column{c.Name, c.Type}
	}
	out["columns"] = columns
	p := res.Rows.Progress()
	out["rows"] = p.Rows
	switch {
	case !p.Done:
		out["status"] = "fetching"
	case p.Err != nil:
		out["status"] = "error"
		out["error"] = map[string]any{"message": "fetching stopped: " + db.Describe(p.Err).Message}
	case p.Stopped == db.StopLimit:
		out["status"] = "done"
		out["incomplete"] = "stopped at the memory limit; there are more rows"
	case p.Stopped == db.StopCancelled:
		out["status"] = "done"
		out["incomplete"] = "the user stopped fetching; there are more rows"
	default:
		out["status"] = "done"
	}
	return out
}

// agentContext resolves the connection and database a tab should run on.
func (m *Model) agentContext(a agentArgs) (queryContext, error) {
	conn, err := m.agentConnection(a.Connection)
	if err != nil {
		return queryContext{}, err
	}
	ctx := queryContext{conn: conn}
	if conn.cfg().Driver.HasDatabases() {
		ctx.database = a.Database
	}
	return ctx, nil
}

func (m *Model) agentOpen(a agentArgs) (any, error) {
	if strings.TrimSpace(a.SQL) == "" {
		return nil, errors.New("sql is required")
	}
	ctx, err := m.agentContext(a)
	if err != nil {
		return nil, err
	}
	q := &m.query
	t := q.newTab(ctx)
	t.name, t.agent = agentTitle(a.Title, q.tabs), true
	t.ed.SetText(a.SQL)
	t.agentVersion = t.ed.Version()
	// An untouched empty tab is replaced rather than kept around.
	if cur := q.current(); cur != nil && cur.path == "" && !cur.dirty() && cur.ed.Text() == "" {
		cur.close()
		q.tabs[q.active] = t
		q.selectTab(q.active)
	} else {
		q.insert(t)
	}
	m.showAgentTab(t, "wrote")
	return map[string]any{"tab": t.id, "title": t.title(),
		"note": "Not executed: the user reviews the tab and runs it."}, nil
}

func (m *Model) agentUpdate(a agentArgs) (any, error) {
	if a.Tab == nil {
		return nil, errors.New("tab is required")
	}
	t, err := m.agentTab(a.Tab)
	if err != nil {
		return nil, err
	}
	if !t.agent {
		return nil, errors.New("this tab is the user's: only tabs you opened can be changed; open a new tab instead")
	}
	if t.ed.Version() != t.agentVersion {
		return nil, errors.New("the user has edited this tab since you wrote it; open a new tab, or ask the user")
	}
	if strings.TrimSpace(a.SQL) == "" {
		return nil, errors.New("sql is required")
	}
	if a.Connection != "" {
		ctx, err := m.agentContext(a)
		if err != nil {
			return nil, err
		}
		t.setContext(ctx)
	}
	t.ed.ReplaceText(a.SQL)
	t.agentVersion = t.ed.Version()
	q := &m.query
	q.selectTab(slices.Index(q.tabs, t))
	m.showAgentTab(t, "revised")
	return map[string]any{"tab": t.id, "title": t.title(),
		"note": "Not executed: the user reviews the tab and runs it."}, nil
}

// showAgentTab brings the agent's tab to the user, with a reminder that
// running it is up to them.
func (m *Model) showAgentTab(t *tab, verb string) {
	if m.modal == nil && m.help == nil {
		m.setFocus(focusQuery)
	}
	m.query.notice = accentStyle.Render(iconAgent.String()+" The agent "+verb+" this query") +
		mutedStyle.Render(" · review it, then "+m.query.runKey+" to run")
}

// agentTitle makes a tab name from the agent's title: one short line,
// unique among the tabs.
func agentTitle(title string, tabs []*tab) string {
	title = strings.Join(strings.Fields(title), " ")
	if r := []rune(title); len(r) > 32 {
		title = strings.TrimSpace(string(r[:32]))
	}
	if title == "" {
		title = agentTabName
	}
	taken := map[string]bool{}
	for _, t := range tabs {
		taken[t.title()] = true
	}
	name := title
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", title, i)
	}
	return name
}
