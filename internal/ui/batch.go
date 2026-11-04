package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"rimor.dev/internal/db"
)

// A SQL Server batch can return several result sets and messages (PRINT,
// SET STATISTICS output, row counts, errors). The results pane shows one
// result at a time ([ and ] switch) and the messages instead with m.

// runMessages are the run's messages, and how many more were dropped.
func runMessages(r *run) ([]db.Message, int) {
	if r == nil || r.res == nil || r.res.Batch == nil {
		return nil, 0
	}
	return r.res.Batch.Messages()
}

// currentRun is the current tab's last run, or nil.
func (m *Model) currentRun() *run {
	if t := m.query.current(); t != nil {
		return t.run
	}
	return nil
}

// switchSet shows the next or previous result set.
func (r *run) switchSet(delta int) {
	n := len(r.res.Sets())
	if n < 2 {
		return
	}
	r.cur = (r.cur + delta + n) % n
	r.messages = false
}

// firstErrorWithoutRows is the error to show instead of results: the
// batch's first error when no result has a row, or a fetch that failed
// before its first row.
func firstErrorWithoutRows(res *db.Result) error {
	sets := res.Sets()
	for _, s := range sets {
		if s.Len() > 0 {
			return nil
		}
	}
	if res.Batch != nil {
		return res.Batch.Err()
	}
	if len(sets) == 1 {
		return sets[0].Progress().Err
	}
	return nil
}

// batchStatus adds what the batch says beyond the result shown: that it
// failed somewhere, or stopped at the memory limit.
func batchStatus(r *run) string {
	b := r.res.Batch
	if b == nil || !b.Done() {
		return ""
	}
	switch {
	case b.Err() != nil:
		return errorStyle.Render(" · ✗ error (m)")
	case b.Stopped() == db.StopLimit:
		return mutedStyle.Render(" · batch stopped at the limit")
	}
	return ""
}

// batchHints are the keys that apply to a batch: switching results and the
// messages.
func batchHints(r *run, focused bool) string {
	if !focused {
		return ""
	}
	var keys []string
	if len(r.res.Sets()) > 1 && !r.messages {
		keys = append(keys, "[ ]", "result")
	}
	if msgs, _ := runMessages(r); len(msgs) > 0 {
		if r.messages {
			keys = append(keys, "m", "results", "y", "copy")
		} else {
			keys = append(keys, "m", fmt.Sprintf("messages %d", len(msgs)))
		}
	}
	return hints(keys...)
}

// messageLines writes the messages as lines of at most width cells:
// errors in red, row counts muted, with a line before each new result.
func messageLines(r *run, width int) []string {
	msgs, dropped := runMessages(r)
	var out []string
	result := 0
	for _, m := range msgs {
		if m.Result != result {
			result = m.Result
			out = append(out, mutedStyle.Render(fmt.Sprintf("── after result %d ", result)+strings.Repeat("─", max(width-20, 0))))
		}
		switch m.Kind {
		case db.MessageRows:
			out = append(out, mutedStyle.Render("("+plural(m.Count, "row")+" affected)"))
		case db.MessageError:
			for _, l := range strings.Split(m.Text, "\n") {
				for _, w := range wrapLines(l, width, 1<<20) {
					out = append(out, errorStyle.Render(w))
				}
			}
		default:
			for _, l := range strings.Split(m.Text, "\n") {
				for _, w := range wrapLines(l, width, 1<<20) {
					out = append(out, textStyle.Render(w))
				}
			}
		}
	}
	if dropped > 0 {
		out = append(out, mutedStyle.Italic(true).Render(fmt.Sprintf("… and %s more, not kept", plural(int64(dropped), "message"))))
	}
	return out
}

// messagesText is the messages as plain text, for copying.
func messagesText(r *run) string {
	msgs, _ := runMessages(r)
	var b strings.Builder
	for _, m := range msgs {
		switch m.Kind {
		case db.MessageRows:
			fmt.Fprintf(&b, "(%s affected)\n", plural(m.Count, "row"))
		default:
			b.WriteString(m.Text + "\n")
		}
	}
	return b.String()
}

func (rp *ResultsPane) messagesView(r *run) string {
	width := max(rp.width-2, 10)
	lines := messageLines(r, width)
	r.msgTop = min(max(r.msgTop, 0), max(len(lines)-rp.height, 0))
	end := min(r.msgTop+rp.height, len(lines))
	rows := make([]string, 0, rp.height)
	for _, l := range lines[r.msgTop:end] {
		rows = append(rows, " "+l)
	}
	return strings.Join(rows, "\n")
}

// messagesKey scrolls the messages and copies them.
func (rp *ResultsPane) messagesKey(msg tea.KeyPressMsg, r *run, page int) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		r.msgTop--
	case "down", "j":
		r.msgTop++
	case "pgup", "ctrl+u":
		r.msgTop -= page
	case "pgdown", "ctrl+d":
		r.msgTop += page
	case "g", "ctrl+home":
		r.msgTop = 0
	case "G", "ctrl+end":
		r.msgTop = 1 << 30
	case "y":
		rp.notice = okStyle.Render("✓ messages copied")
		return tea.SetClipboard(messagesText(r))
	case "esc":
		r.messages = false
	}
	return nil
}
