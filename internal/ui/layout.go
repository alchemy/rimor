package ui

import (
	"math"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Default proportions, restored by alt+= or a double click on a handle.
const (
	defaultExplorerFrac = 0.25
	defaultQueryFrac    = 0.35
	defaultChatFrac     = 0.3
)

// Smallest sizes a pane can be dragged to, borders included.
const (
	minExplorerWidth = 16
	minRightWidth    = 40
	minQueryHeight   = 4
	minResultsHeight = 5
	minChatWidth     = 30
)

// Steps for keyboard resizing.
const (
	resizeStepCols = 2
	resizeStepRows = 1
)

const doubleClick = 400 * time.Millisecond

// handle is a draggable border between panes.
type handle int

const (
	handleNone handle = iota
	handleExplorer
	handleQuery
	handleChat
)

// layout holds the split positions as fractions of the screen so they
// survive terminal resizes.
type layout struct {
	explorerFrac float64 // explorer width / screen width
	queryFrac    float64 // query height / screen height
	chatFrac     float64 // chat width / screen width

	// full shows only the focused pane, filling the screen.
	full bool

	hover handle
	drag  handle
	grab  int // offset from the border to where the drag started

	lastClick   time.Time
	lastClicked handle
}

func defaultLayout() layout {
	return layout{explorerFrac: defaultExplorerFrac, queryFrac: defaultQueryFrac, chatFrac: defaultChatFrac}
}

// clampSplit keeps a split between min sizes on both sides, sharing the
// space evenly when the screen is too small for both minimums.
func clampSplit(v, total, minBefore, minAfter int) int {
	if total < minBefore+minAfter {
		return total * minBefore / (minBefore + minAfter)
	}
	return min(max(v, minBefore), total-minAfter)
}

// layout returns the explorer width and the query and results heights.
// The query and results panes take the width the explorer and the chat
// leave.
func (m Model) layout() (leftW, topH, bottomH int) {
	leftW = clampSplit(int(math.Round(float64(m.width)*m.split.explorerFrac)), m.width-m.chatWidth(), minExplorerWidth, minRightWidth)
	topH = clampSplit(int(math.Round(float64(m.height)*m.split.queryFrac)), m.height, minQueryHeight, minResultsHeight)
	return leftW, topH, m.height - topH
}

// chatWidth is the width of the chat column on the right.
func (m Model) chatWidth() int {
	w := int(math.Round(float64(m.width) * m.split.chatFrac))
	return clampSplit(w, m.width-minExplorerWidth, minChatWidth, minRightWidth)
}

// setChatWidth moves the chat border so the chat is chatW columns wide.
func (m *Model) setChatWidth(chatW int) {
	chatW = clampSplit(chatW, m.width-minExplorerWidth, minChatWidth, minRightWidth)
	m.split.chatFrac = float64(chatW) / float64(m.width)
	m.resize()
}

// setExplorerWidth moves the explorer border to leftW columns.
func (m *Model) setExplorerWidth(leftW int) {
	leftW = clampSplit(leftW, m.width-m.chatWidth(), minExplorerWidth, minRightWidth)
	m.split.explorerFrac = float64(leftW) / float64(m.width)
	m.resize()
}

// setQueryHeight moves the query/results border to topH rows.
func (m *Model) setQueryHeight(topH int) {
	topH = clampSplit(topH, m.height, minQueryHeight, minResultsHeight)
	m.split.queryFrac = float64(topH) / float64(m.height)
	m.resize()
}

// resize passes the current pane sizes down to the panes.
func (m *Model) resize() {
	if m.split.full {
		// Only the focused pane shows; sizing them all lets focus move
		// between panes without another resize.
		m.explorer.SetSize(m.width-2, m.height-2)
		m.query.SetSize(m.width-2, m.height-2)
		m.results.SetSize(m.width-2, m.height-2)
		m.chat.SetSize(m.width-2, m.height-2)
		return
	}
	leftW, topH, bottomH := m.layout()
	chatW := m.chatWidth()
	m.explorer.SetSize(leftW-2, m.height-2)
	m.query.SetSize(m.width-leftW-chatW-2, topH-2)
	m.results.SetSize(m.width-leftW-chatW-2, bottomH-2)
	m.chat.SetSize(max(chatW-2, 0), m.height-2)
}

// handleAt finds the handle under a cell. Each handle is the pair of
// border lines where two panes meet.
func (m Model) handleAt(x, y int) handle {
	if m.split.full {
		return handleNone
	}
	leftW, topH, _ := m.layout()
	chatX := m.width - m.chatWidth() // the chat's left edge
	switch {
	case x == leftW-1 || x == leftW:
		return handleExplorer
	case chatX < m.width && (x == chatX-1 || x == chatX):
		return handleChat
	case x > leftW && x < chatX-1 && (y == topH-1 || y == topH):
		return handleQuery
	}
	return handleNone
}

// paneAt finds the pane under a cell.
func (m Model) paneAt(x, y int) focus {
	if m.split.full {
		return m.focus
	}
	leftW, topH, _ := m.layout()
	switch {
	case x < leftW:
		return focusExplorer
	case x >= m.width-m.chatWidth():
		return focusChat
	case y < topH:
		return focusQuery
	}
	return focusResults
}

// resizeAction runs a keyboard resize action.
func (m *Model) resizeAction(action string) {
	if m.split.full {
		return // nothing to resize
	}
	leftW, topH, _ := m.layout()
	switch action {
	case "shrink_explorer":
		m.setExplorerWidth(leftW - resizeStepCols)
	case "grow_explorer":
		m.setExplorerWidth(leftW + resizeStepCols)
	case "shrink_query":
		m.setQueryHeight(topH - resizeStepRows)
	case "grow_query":
		m.setQueryHeight(topH + resizeStepRows)
	case "reset_layout":
		m.split.explorerFrac, m.split.queryFrac, m.split.chatFrac = defaultExplorerFrac, defaultQueryFrac, defaultChatFrac
		m.resize()
	}
	m.saveSession()
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	mouse := msg.Mouse()
	leftW, topH, _ := m.layout()

	switch msg.(type) {
	case tea.MouseClickMsg:
		if m.modal != nil || mouse.Button != tea.MouseLeft {
			return nil
		}
		h := m.handleAt(mouse.X, mouse.Y)
		if h == handleNone {
			m.setFocus(m.paneAt(mouse.X, mouse.Y))
			return nil
		}
		chatX := m.width - m.chatWidth()
		// A second click on the same handle resets that split.
		now := time.Now()
		if h == m.split.lastClicked && now.Sub(m.split.lastClick) < doubleClick {
			m.split.lastClicked = handleNone
			switch h {
			case handleExplorer:
				m.split.explorerFrac = defaultExplorerFrac
			case handleChat:
				m.split.chatFrac = defaultChatFrac
			default:
				m.split.queryFrac = defaultQueryFrac
			}
			m.resize()
			m.saveSession()
			return nil
		}
		m.split.lastClick, m.split.lastClicked = now, h
		m.split.drag = h
		switch h {
		case handleExplorer:
			m.split.grab = mouse.X - leftW
		case handleChat:
			m.split.grab = mouse.X - chatX
		default:
			m.split.grab = mouse.Y - topH
		}

	case tea.MouseMotionMsg:
		if m.split.drag != handleNone {
			m.split.lastClicked = handleNone // a drag is not half of a double click
		}
		switch m.split.drag {
		case handleExplorer:
			m.setExplorerWidth(mouse.X - m.split.grab)
		case handleQuery:
			m.setQueryHeight(mouse.Y - m.split.grab)
		case handleChat:
			m.setChatWidth(m.width - (mouse.X - m.split.grab))
		default:
			if m.modal == nil {
				m.split.hover = m.handleAt(mouse.X, mouse.Y)
			}
		}

	case tea.MouseWheelMsg:
		if m.modal == nil && m.paneAt(mouse.X, mouse.Y) == focusChat {
			switch mouse.Button {
			case tea.MouseWheelUp:
				m.chat.scroll(3)
			case tea.MouseWheelDown:
				m.chat.scroll(-3)
			}
		}

	case tea.MouseReleaseMsg:
		if m.split.drag != handleNone {
			m.split.drag = handleNone
			m.split.hover = m.handleAt(mouse.X, mouse.Y)
			m.saveSession()
		}
	}
	return nil
}

// hot is the handle drawn highlighted: the one being dragged, else the one
// under the mouse.
func (l layout) hot() handle {
	if l.drag != handleNone {
		return l.drag
	}
	return l.hover
}
