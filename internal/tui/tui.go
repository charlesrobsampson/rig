// Package tui is rig's terminal interface: a Bubble Tea app that is a thin client of
// the daemon, like the CLI. This file is the root model: layout, keymap, status bar,
// the help overlay, and the connection to the daemon with its "not running" state.
// The views inside it (dashboard, detail, ...) arrive in later tasks.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charlesrobsampson/rig/internal/api"
)

// Source is where the TUI gets its data. The daemon-backed implementation lives in
// cmd/rig; tests use a fake.
type Source interface {
	// Tickets returns the fleet. An error means the daemon could not be reached.
	Tickets(ctx context.Context) ([]api.TicketSummary, error)
}

type Options struct {
	Source  Source
	Version string
	// RetryEvery is how long to wait before looking for a daemon that is not running
	// (default 2s).
	RetryEvery time.Duration
}

// Messages. They are exported only to the package's own tests.
type (
	// loadedMsg is the result of asking the source for the fleet.
	loadedMsg struct {
		tickets []api.TicketSummary
		err     error
	}
	// retryMsg says it is time to look for the daemon again.
	retryMsg struct{}
)

type Model struct {
	opts Options

	width, height int
	help          bool
	quitting      bool

	connected bool // the last attempt reached the daemon
	loaded    bool // there has been at least one attempt
	err       string
	retrying  bool // a retry timer is already running
	tickets   []api.TicketSummary
	dash      *dashboard
}

func New(o Options) Model {
	if o.RetryEvery == 0 {
		o.RetryEvery = 2 * time.Second
	}
	return Model{opts: o, dash: &dashboard{}}
}

// Init asks for the fleet at once.
func (m Model) Init() tea.Cmd { return m.load() }

func (m Model) load() tea.Cmd {
	src := m.opts.Source
	return func() tea.Msg {
		if src == nil {
			return loadedMsg{err: fmt.Errorf("no daemon connection configured")}
		}
		tickets, err := src.Tickets(context.Background())
		return loadedMsg{tickets: tickets, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)

	case loadedMsg:
		m.loaded = true
		if msg.err != nil {
			m.connected, m.err = false, msg.err.Error()
			if m.retrying {
				return m, nil
			}
			m.retrying = true
			return m, tea.Tick(m.opts.RetryEvery, func(time.Time) tea.Msg { return retryMsg{} })
		}
		m.connected, m.err, m.tickets = true, "", msg.tickets
		m.dash.set(msg.tickets)
		return m, nil

	case retryMsg:
		m.retrying = false
		if m.connected {
			return m, nil
		}
		return m, m.load()
	}
	return m, nil
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc":
		m.help = false
		return m, nil
	case "?":
		m.help = !m.help
		return m, nil
	}
	if m.help {
		return m, nil // the overlay swallows everything else until it is closed
	}
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "r":
		return m, m.load()
	case "down", "j":
		m.dash.move(1)
	case "up", "k":
		m.dash.move(-1)
	case "home", "g":
		m.dash.first()
	case "end", "G":
		m.dash.last()
	}
	return m, nil
}

// Keys documents the keymap; the help overlay and the status bar render from it.
var Keys = []struct{ Key, Help string }{
	{"?", "show or hide this help"},
	{"j/k ↑/↓", "move the selection"},
	{"g/G", "first / last ticket"},
	{"r", "refresh now"},
	{"q", "quit"},
	{"ctrl+c", "quit from anywhere"},
	{"esc", "close help"},
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	header := m.header(w)
	status := m.statusBar(w)
	if h < 3 {
		return status // no room for anything but the status bar
	}
	bodyH := h - 2
	var body string
	switch {
	case m.help:
		body = m.helpView()
	case !m.loaded:
		body = "connecting to the daemon…"
	case !m.connected:
		body = m.downView()
	default:
		body = m.dash.view(w, bodyH)
	}
	return strings.Join([]string{header, fit(body, w, bodyH), status}, "\n")
}

func (m Model) header(w int) string {
	title := "rig"
	if m.opts.Version != "" {
		title += " " + m.opts.Version
	}
	return ansi.Truncate(title, w, "…")
}

func (m Model) statusBar(w int) string {
	var s string
	switch {
	case !m.loaded:
		s = "connecting…"
	case !m.connected:
		s = "daemon not running · r retry · q quit"
	default:
		s = fmt.Sprintf("connected · %d tickets · ? help · q quit", len(m.tickets))
	}
	return ansi.Truncate(s, w, "…")
}

func (m Model) downView() string {
	var b strings.Builder
	b.WriteString("The rig daemon is not running.\n\n")
	if m.err != "" {
		b.WriteString(m.err + "\n\n")
	}
	fmt.Fprintf(&b, "Looking again every %s; press r to try now.", m.opts.RetryEvery)
	return b.String()
}

func (m Model) helpView() string {
	lines := []string{"Keys", ""}
	for _, k := range Keys {
		lines = append(lines, fmt.Sprintf("  %-8s %s", k.Key, k.Help))
	}
	return strings.Join(lines, "\n")
}

// fit cuts text to w columns and h lines, so a small terminal never overflows or
// panics.
func fit(text string, w, h int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "…")
	}
	return strings.Join(lines, "\n")
}

// Run shows the TUI until the user quits.
func Run(ctx context.Context, o Options) error {
	_, err := tea.NewProgram(New(o), tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}
