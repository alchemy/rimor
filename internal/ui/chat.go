package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/acp"
	"rimor.dev/internal/config"
	"rimor.dev/internal/editor"
)

// The agent chat: a pane where the user talks to an AI agent (Claude,
// Codex, Gemini…) that rimor runs through the Agent Client Protocol. The
// agent's only tools are rimor's own, through rimor mcp: it reads the
// catalog and opens query tabs, which the user reviews and runs, as with
// an agent in another terminal. See chat_session.go.

type chatPhase int

const (
	chatPicking  chatPhase = iota // choosing an agent
	chatStarting                  // starting it and opening a session
	chatReady                     // waiting for the user's message
	chatBusy                      // the agent is answering
	chatFailed                    // it could not start, or it stopped
)

type entryKind int

const (
	entryUser entryKind = iota
	entryAgent
	entryThought
	entryTool
	entryPlan
	entryInfo  // rimor's notes: connected, refused tool calls
	entryError // failures
)

type chatEntry struct {
	kind entryKind
	text string

	// Tool calls: what the agent reported so far, the rimor tool it runs
	// ("" for any other), and its status: pending, in_progress, completed
	// or failed.
	call   acp.Update
	tool   string
	status string

	// lines is the entry rendered at width; text changes reset width.
	lines []string
	width int
}

// Input height, in lines: it grows with the text up to the maximum.
const (
	chatInputMin = 2
	chatInputMax = 6
)

// ChatPane is the agent chat.
type ChatPane struct {
	agents  []config.ChatAgent
	pick    int       // the agent under the picker's cursor
	phase   chatPhase //
	started bool      // an agent was started since rimor opened

	agent   config.ChatAgent // the agent running or starting
	label   string           // its name and version, once connected
	sess    *chatSession
	entries []chatEntry
	plan    int // index of the current turn's plan entry, or -1

	input *editor.Model
	back  int // lines scrolled back from the end; 0 follows the chat

	width, height int
	focused       bool

	socket  string // rimor's agent socket; "" when agents cannot reach it
	why     string // why they cannot, for the picker
	version string // rimor's, for the agent
	start   startFunc
}

func newChatPane(s config.Settings) ChatPane {
	c := ChatPane{agents: s.ChatAgentList(), plan: -1, start: startProcess, input: editor.New(editorTheme),
		why: "Agent access is off: set agent = true in config.toml to use the chat."}
	for i, a := range c.agents {
		if a.Name == s.ChatAgent {
			c.pick = i
		}
	}
	c.input.NoLineNumbers = true
	c.input.SetLanguage(editor.Plain)
	c.input.Placeholder = "Ask the agent about your databases…"
	return c
}

// typing reports that the pane takes text, as the editor does.
func (c *ChatPane) typing() bool { return c.phase != chatPicking }

func (c *ChatPane) SetSize(width, height int) {
	c.width, c.height = width, height
	c.input.SetSize(width, c.inputHeight())
}

func (c *ChatPane) inputHeight() int {
	return min(max(c.input.LineCount(), chatInputMin), chatInputMax, max(c.height/3, 1))
}

func (c *ChatPane) Focus() {
	c.focused = true
	c.input.Focus()
}

func (c *ChatPane) Blur() {
	c.focused = false
	c.input.Blur()
}

// Cursor is the terminal cursor inside the pane body.
func (c *ChatPane) Cursor() (x, y int, ok bool) {
	if !c.typing() || !c.focused {
		return 0, 0, false
	}
	x, y, ok = c.input.Cursor()
	return x, c.height - c.inputHeight() + y, ok
}

// autoStart starts the configured agent the first time the chat has the
// focus, so that rimor starts no agent the user does not talk to.
func (c *ChatPane) autoStart() tea.Cmd {
	if c.started || c.phase != chatPicking || c.socket == "" || len(c.agents) == 0 {
		return nil
	}
	return c.startAgent(c.agents[c.pick])
}

// Close ends the agent, when rimor quits.
func (c *ChatPane) Close() {
	if c.sess != nil {
		c.sess.close()
	}
}

// leave ends the current session, if any, without waiting for it.
func (c *ChatPane) leave() {
	if s := c.sess; s != nil {
		go s.close()
	}
	c.sess = nil
}

func (c *ChatPane) startAgent(a config.ChatAgent) tea.Cmd {
	c.leave()
	s := newChatSession()
	c.sess, c.agent, c.label, c.phase, c.started = s, a, a.Label(), chatStarting, true
	c.entries, c.plan, c.back = nil, -1, 0
	c.add(chatEntry{kind: entryInfo, text: "Starting " + a.Label() + " (" + strings.Join(a.Command, " ") + ")…"})
	socket, version, start := c.socket, c.version, c.start
	return tea.Batch(s.next(), func() tea.Msg {
		s.begin(a, socket, version, start)
		return nil
	})
}

func (c *ChatPane) add(e chatEntry) {
	c.entries = append(c.entries, e)
}

// last is the last entry, if it is of the given kind.
func (c *ChatPane) last(kind entryKind) *chatEntry {
	if n := len(c.entries); n > 0 && c.entries[n-1].kind == kind {
		return &c.entries[n-1]
	}
	return nil
}

// ── Session events ──────────────────────────────────────────────────────────

// Event handles a message from the agent's session.
func (c *ChatPane) Event(msg chatMsg) tea.Cmd {
	s := msg.session()
	if s != c.sess || s == nil {
		if p, ok := msg.(chatPermissionMsg); ok {
			p.reply <- "" // a session the user left
		}
		return nil
	}
	next := s.next()
	switch msg := msg.(type) {
	case chatReadyMsg:
		if msg.err != nil {
			err := msg.err
			if errors.Is(err, acp.ErrNotInstalled) && c.agent.Install != "" {
				err = fmt.Errorf("%w. Install it with: %s (or set its command in config.toml)", err, c.agent.Install)
			}
			c.fail("Could not start "+c.agent.Label()+": ", err)
			return nil // the session is over
		}
		c.phase, c.label = chatReady, msg.label
		c.add(chatEntry{kind: entryInfo, text: "Connected to " + msg.label + ". It can read your catalog and open query tabs; it cannot run SQL: you run what it writes."})
	case chatUpdateMsg:
		c.update(msg.u)
	case chatPermissionMsg:
		option, allowed := permissionChoice(msg.req)
		msg.reply <- option
		if !allowed {
			c.add(chatEntry{kind: entryInfo, text: "Refused: " + toolTitle(msg.req.ToolCall) + ". The chat allows rimor's tools only."})
		}
	case chatTurnMsg:
		c.phase = chatReady
		c.plan = -1
		switch {
		case msg.err != nil:
			c.add(chatEntry{kind: entryError, text: sanitize(msg.err.Error())})
		case msg.stop == acp.StopCancelled:
			c.add(chatEntry{kind: entryInfo, text: "Stopped."})
		case msg.stop == acp.StopRefusal:
			c.add(chatEntry{kind: entryInfo, text: "The agent declined to answer."})
		case msg.stop == acp.StopMaxTokens, msg.stop == acp.StopMaxTurns:
			c.add(chatEntry{kind: entryInfo, text: "The agent stopped at its limit (" + msg.stop + ")."})
		}
	case chatExitMsg:
		if c.phase != chatFailed {
			c.fail(c.agent.Label()+" stopped: ", withStderr(msg.err, msg.stderr))
		}
		return nil
	}
	return next
}

func (c *ChatPane) fail(prefix string, err error) {
	c.phase = chatFailed
	c.add(chatEntry{kind: entryError, text: prefix + sanitize(err.Error())})
	c.leave()
}

func (c *ChatPane) update(u acp.Update) {
	switch u.Kind {
	case acp.UpdateAgentMessage, acp.UpdateAgentThought:
		kind := entryAgent
		if u.Kind == acp.UpdateAgentThought {
			kind = entryThought
		}
		b := u.Chunk()
		text := b.Text
		if b.Type != "text" {
			text = "[" + b.Type + "]"
		}
		if e := c.last(kind); e != nil {
			e.text += sanitize(text)
			e.width = 0
			return
		}
		c.add(chatEntry{kind: kind, text: strings.TrimLeft(sanitize(text), "\n")})
	case acp.UpdateToolCall:
		e := chatEntry{kind: entryTool, status: "pending"}
		e.merge(u)
		c.add(e)
	case acp.UpdateToolCallEdit:
		for i := len(c.entries) - 1; i >= 0; i-- {
			if e := &c.entries[i]; e.kind == entryTool && e.call.ToolCallID == u.ToolCallID {
				e.merge(u)
				return
			}
		}
	case acp.UpdatePlan:
		var b strings.Builder
		for _, p := range u.Entries {
			mark := "☐"
			switch p.Status {
			case "completed":
				mark = "☑"
			case "in_progress":
				mark = "▸"
			}
			fmt.Fprintf(&b, "%s %s\n", mark, sanitize(p.Content))
		}
		e := chatEntry{kind: entryPlan, text: strings.TrimSuffix(b.String(), "\n")}
		if c.plan >= 0 && c.plan < len(c.entries) {
			c.entries[c.plan] = e
			return
		}
		c.plan = len(c.entries)
		c.add(e)
	case acp.UpdateNotice:
		kind := entryInfo
		if u.Severity == "error" {
			kind = entryError
		}
		if u.Title != nil {
			c.add(chatEntry{kind: kind, text: sanitize(*u.Title)})
		}
	}
}

// merge applies a tool call's update: only what changed is sent.
func (e *chatEntry) merge(u acp.Update) {
	c := &e.call
	c.ToolCallID = u.ToolCallID
	if u.Name != nil {
		c.Name = u.Name
	}
	if u.Title != nil {
		c.Title = u.Title
	}
	if u.ToolKind != nil {
		c.ToolKind = u.ToolKind
	}
	if len(u.RawInput) > 0 && string(u.RawInput) != "null" {
		c.RawInput = u.RawInput
	}
	if u.Status != nil {
		e.status = *u.Status
	}
	e.tool, e.text = rimorTool(*c), toolTitle(*c)
	if e.status == "failed" {
		if out := strings.TrimSpace(u.ToolContent()); out != "" {
			e.text += ": " + firstLine(sanitize(out))
		}
	}
	e.width = 0
}

// toolTitle describes a tool call: rimor's tools by name and their main
// arguments, others by the title the agent gives.
func toolTitle(u acp.Update) string {
	tool := rimorTool(u)
	if tool == "" {
		switch {
		case u.Title != nil && *u.Title != "":
			return firstLine(sanitize(strings.TrimSpace(*u.Title)))
		case u.Name != nil:
			return sanitize(*u.Name)
		}
		return "a tool call"
	}
	var args map[string]any
	_ = json.Unmarshal(u.RawInput, &args)
	var parts []string
	for _, k := range []string{"connection", "database", "schema", "name", "text", "title", "tab"} {
		switch v := args[k].(type) {
		case string:
			if v != "" {
				parts = append(parts, v)
			}
		case float64:
			parts = append(parts, fmt.Sprintf("tab %d", int(v)))
		}
	}
	if len(parts) == 0 {
		return tool
	}
	return tool + "  " + sanitize(strings.Join(parts, " · "))
}

// sanitize drops escape sequences and other control characters from
// what the agent sends: it must not drive the terminal.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// ── Keys ────────────────────────────────────────────────────────────────────

// Update handles keys and pastes. ctx describes where the user is in
// rimor, for the agent.
func (c *ChatPane) Update(msg tea.Msg, ctx string) tea.Cmd {
	key, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		if !c.typing() {
			return nil
		}
		cmd := c.input.Update(msg) // a paste
		c.SetSize(c.width, c.height)
		return cmd
	}
	if !c.typing() {
		return c.pickerKey(key.String())
	}
	switch key.String() {
	case "enter":
		return c.send(ctx)
	case "shift+enter", "ctrl+j":
		cmd := c.input.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		c.SetSize(c.width, c.height)
		return cmd
	case "esc":
		if c.phase == chatBusy {
			c.sess.cancel()
			return nil
		}
	case "pgup":
		c.scroll(max(c.transcriptHeight()/2, 1))
		return nil
	case "pgdown":
		c.scroll(-max(c.transcriptHeight()/2, 1))
		return nil
	case "ctrl+n":
		c.leave()
		c.phase, c.back = chatPicking, 0
		return nil
	}
	cmd := c.input.Update(msg)
	c.SetSize(c.width, c.height)
	return cmd
}

func (c *ChatPane) pickerKey(key string) tea.Cmd {
	switch key {
	case "up", "k":
		c.pick = max(c.pick-1, 0)
	case "down", "j":
		c.pick = min(c.pick+1, len(c.agents)-1)
	case "enter":
		if c.socket != "" && len(c.agents) > 0 {
			return c.startAgent(c.agents[c.pick])
		}
	}
	return nil
}

// send gives the user's message to the agent.
func (c *ChatPane) send(ctx string) tea.Cmd {
	text := strings.TrimSpace(c.input.Text())
	if text == "" || c.phase != chatReady {
		return nil
	}
	c.input.SetText("")
	c.SetSize(c.width, c.height)
	c.add(chatEntry{kind: entryUser, text: text})
	c.phase, c.back = chatBusy, 0
	blocks := []acp.ContentBlock{acp.Text(text)}
	if ctx != "" {
		blocks = append(blocks, acp.Text("(From rimor, not typed by the user: "+ctx+")"))
	}
	s := c.sess
	return func() tea.Msg {
		s.prompt(blocks)
		return nil
	}
}

// ── View ────────────────────────────────────────────────────────────────────

func (c *ChatPane) transcriptHeight() int {
	return max(c.height-c.inputHeight()-1, 1)
}

// transcript is the conversation's lines at the pane's width.
func (c *ChatPane) transcript() []string {
	var lines []string
	for i := range c.entries {
		e := &c.entries[i]
		if i > 0 && (e.kind == entryUser || e.kind == entryAgent && c.entries[i-1].kind != entryAgent ||
			e.kind == entryInfo || e.kind == entryError) {
			lines = append(lines, "")
		}
		lines = append(lines, e.render(c.width)...)
	}
	return lines
}

// scroll moves back through the conversation by delta lines, or forward.
func (c *ChatPane) scroll(delta int) {
	c.back = min(max(c.back+delta, 0), max(len(c.transcript())-c.transcriptHeight(), 0))
}

func (c *ChatPane) View() string {
	if !c.typing() {
		return c.pickerView()
	}
	lines := c.transcript()
	h := c.transcriptHeight()
	c.back = min(c.back, max(len(lines)-h, 0))
	end := len(lines) - c.back
	lines = lines[max(end-h, 0):end]
	for len(lines) < h {
		lines = append(lines, "")
	}
	sep := mutedStyle.Render(strings.Repeat("─", c.width))
	if c.back > 0 {
		more := fmt.Sprintf(" ↓ %d more ", c.back)
		sep = mutedStyle.Render(strings.Repeat("─", max(c.width-len(more)-1, 0))) + accentStyle.Render(more) + mutedStyle.Render("─")
	}
	return strings.Join(lines, "\n") + "\n" + sep + "\n" + c.input.View()
}

func (c *ChatPane) pickerView() string {
	if c.socket == "" {
		return "\n" + wrapStyled(c.why, c.width, mutedStyle, "  ")
	}
	if len(c.agents) == 0 {
		return "\n" + wrapStyled("No chat agents: add one under [chat_agents] in config.toml.", c.width, mutedStyle, "  ")
	}
	var b strings.Builder
	b.WriteString("\n  " + titleStyle.Render("Choose an agent") + "\n\n")
	w := 0
	for _, a := range c.agents {
		w = max(w, lipgloss.Width(a.Label()))
	}
	for i, a := range c.agents {
		label := a.Label() + strings.Repeat(" ", w-lipgloss.Width(a.Label()))
		cmd := strings.Join(a.Command, " ")
		if i == c.pick {
			b.WriteString(accentStyle.Render("  › "+label) + "  " + mutedStyle.Render(cmd) + "\n")
		} else {
			b.WriteString("    " + textStyle.Render(label) + "  " + mutedStyle.Render(cmd) + "\n")
		}
	}
	b.WriteString("\n" + wrapStyled("The agent gets rimor's tools only: it reads your catalog and opens query tabs, and you run them. "+
		"Each agent needs its ACP adapter and a login; see config.toml.", c.width, mutedStyle, "  "))
	return b.String()
}

// render draws the entry at width, cached until it changes.
func (e *chatEntry) render(width int) []string {
	if e.width == width && e.lines != nil {
		return e.lines
	}
	e.width = width
	switch e.kind {
	case entryUser:
		e.lines = prefixed(e.text, width, accentStyle.Render("› "), textStyle.Bold(true))
	case entryAgent:
		e.lines = renderMarkdown(e.text, width)
	case entryThought:
		text := strings.ReplaceAll(e.text, "**", "")
		if ls := strings.Split(strings.TrimSpace(text), "\n"); len(ls) > 3 {
			text = strings.Join(ls[:3], "\n") + " …"
		}
		e.lines = prefixed(text, width, mutedStyle.Render("  "), mutedStyle.Italic(true))
	case entryTool:
		mark := mutedStyle.Render("○")
		switch e.status {
		case "in_progress":
			mark = accentStyle.Render("◌")
		case "completed":
			mark = okStyle.Render("✓")
		case "failed":
			mark = errorStyle.Render("✗")
		}
		text := e.text
		if e.tool != "" {
			text = "rimor › " + text
		}
		e.lines = []string{ansi.Truncate("  "+mark+" "+mutedStyle.Render(text), width, "…")}
	case entryPlan:
		e.lines = prefixed(e.text, width, mutedStyle.Render("  "), textStyle)
	case entryInfo:
		e.lines = prefixed(e.text, width, mutedStyle.Render("· "), mutedStyle)
	case entryError:
		e.lines = prefixed(e.text, width, errorStyle.Render("✗ "), errorStyle)
	}
	return e.lines
}

// prefixed wraps text to width after a two-cell prefix, indenting the
// following lines to match.
func prefixed(text string, width int, prefix string, style lipgloss.Style) []string {
	lines := strings.Split(wrapStyled(text, width, style, "  "), "\n")
	if len(lines) > 0 {
		lines[0] = prefix + strings.TrimPrefix(lines[0], "  ")
	}
	return lines
}

// wrapStyled word-wraps text to width, each line indented and styled.
func wrapStyled(text string, width int, style lipgloss.Style, indent string) string {
	w := max(width-lipgloss.Width(indent), 1)
	var out []string
	for _, l := range strings.Split(ansi.Wrap(text, w, ""), "\n") {
		out = append(out, indent+style.Render(l))
	}
	return strings.Join(out, "\n")
}

// renderMarkdown draws the agent's answer: wrapped text, with code blocks
// set apart and kept unwrapped (cut at the edge) so SQL keeps its layout.
func renderMarkdown(text string, width int) []string {
	var out []string
	code := false
	for _, l := range strings.Split(strings.Trim(text, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			code = !code
			continue
		}
		if code {
			out = append(out, ansi.Truncate(mutedStyle.Render("  │ ")+lipgloss.NewStyle().Foreground(colorPeach).Render(l), width, "…"))
			continue
		}
		style := textStyle
		if t, ok := strings.CutPrefix(l, "#"); ok {
			l, style = strings.TrimLeft(t, "# "), textStyle.Bold(true)
		}
		l = strings.ReplaceAll(l, "**", "")
		out = append(out, strings.Split(wrapStyled(l, width, style, "  "), "\n")...)
	}
	return out
}

// ── Borders ─────────────────────────────────────────────────────────────────

// Status describes the agent and what it is doing, for the bottom border.
func (c *ChatPane) Status() string {
	ic := iconStyle(iconAgent).Render(iconAgent.String())
	switch c.phase {
	case chatPicking:
		return ""
	case chatStarting:
		return ic + " " + textStyle.Render(c.agent.Label()) + mutedStyle.Render(" · starting…")
	case chatFailed:
		return ic + " " + textStyle.Render(c.agent.Label()) + errorStyle.Render(" · stopped")
	case chatBusy:
		if c.waitingForRun() {
			return ic + " " + textStyle.Render(c.agent.Label()) + accentStyle.Render(" · waiting for you to run the tab")
		}
		return ic + " " + textStyle.Render(c.agent.Label()) + accentStyle.Render(" · working…")
	}
	return ic + " " + textStyle.Render(c.agent.Label())
}

// waitingForRun reports that the agent waits for the user to run a tab.
func (c *ChatPane) waitingForRun() bool {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if e := c.entries[i]; e.kind == entryTool {
			return e.tool == "wait_for_run" && (e.status == "pending" || e.status == "in_progress")
		}
	}
	return false
}

// Footer shows the keys for the current phase.
func (c *ChatPane) Footer(focused bool) string {
	if !focused {
		return ""
	}
	switch c.phase {
	case chatPicking:
		if c.socket == "" || len(c.agents) == 0 {
			return ""
		}
		return hints("⏎", "start")
	case chatBusy:
		return hints("esc", "stop", "pgup", "scroll")
	case chatReady:
		return hints("⏎", "send", "^j", "new line", "^n", "new chat")
	}
	return hints("^n", "new chat")
}
