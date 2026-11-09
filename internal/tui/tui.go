// Package tui is rig's terminal interface: a Bubble Tea app that is a thin client of
// the daemon, like the CLI. This file is the root model: layout, keymap, status bar,
// the help overlay, and the connection to the daemon with its "not running" state.
// The views inside it (dashboard, detail, ...) arrive in later tasks.
package tui

import (
	"context"
	"errors"
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
	// File reads a lifecycle file of a ticket (plan.md, tasks.md, contract.md);
	// ErrMissing when it does not exist.
	File(ctx context.Context, ticket, name string) (string, error)
	// Diff is what the ticket's services changed, ready to show (ErrMissing when
	// there is nothing to diff yet); Logs is the recent output of its containers.
	Diff(ctx context.Context, ticket string) (string, error)
	Logs(ctx context.Context, ticket string) (string, error)
}

type Options struct {
	Source  Source
	Version string
	// RetryEvery is how long to wait before looking for a daemon that is not running
	// (default 2s).
	RetryEvery time.Duration
	// RefreshEvery is how often the open ticket's current tab is looked at again, so
	// it follows an agent editing the files (default 2s).
	RefreshEvery time.Duration
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
	// tabLoadedMsg is a tab's content arriving.
	tabLoadedMsg struct {
		ticket string
		gen    int
		tab    Tab
		text   string
		err    error
	}
	// refreshMsg says it is time to look at the open tab again.
	refreshMsg struct {
		ticket string
		gen    int
	}
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
	det       *detail // the open ticket, nil on the dashboard
	openGen   int
}

func New(o Options) Model {
	if o.RetryEvery == 0 {
		o.RetryEvery = 2 * time.Second
	}
	if o.RefreshEvery == 0 {
		o.RefreshEvery = 2 * time.Second
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

	case tabLoadedMsg:
		if m.det == nil || m.det.ticket != msg.ticket || m.det.gen != msg.gen {
			return m, nil // for a ticket that is no longer open
		}
		p := page{loaded: true, text: msg.text}
		switch {
		case errors.Is(msg.err, ErrMissing):
			p = page{loaded: true, missing: true}
		case msg.err != nil:
			p = page{loaded: true, err: msg.err.Error()}
		}
		m.det.pages[msg.tab] = p
		return m, nil

	case refreshMsg:
		if m.det == nil || m.det.ticket != msg.ticket || m.det.gen != msg.gen {
			return m, nil
		}
		return m, tea.Batch(m.loadTab(m.det.tab), m.refreshTick())

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
		if m.help {
			m.help = false
			return m, nil
		}
	case "?":
		m.help = !m.help
		return m, nil
	}
	if m.help {
		return m, nil // the overlay swallows everything else until it is closed
	}
	if m.det != nil {
		return m.detailKey(msg)
	}
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "r":
		return m, m.load()
	case "enter", "right", "l":
		if t, ok := m.dash.current(); ok {
			return m.open(t.ID)
		}
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

// open shows a ticket's detail view, loading its first tab.
func (m Model) open(id string) (tea.Model, tea.Cmd) {
	m.openGen++
	m.det = &detail{ticket: id, gen: m.openGen}
	return m, tea.Batch(m.loadTab(TabPlan), m.refreshTick())
}

func (m Model) refreshTick() tea.Cmd {
	id, gen := m.det.ticket, m.det.gen
	return tea.Tick(m.opts.RefreshEvery, func(time.Time) tea.Msg { return refreshMsg{ticket: id, gen: gen} })
}

// loadTab reads one tab of the open ticket from the source.
func (m Model) loadTab(t Tab) tea.Cmd {
	src, id, gen := m.opts.Source, m.det.ticket, m.det.gen
	return func() tea.Msg {
		msg := tabLoadedMsg{ticket: id, gen: gen, tab: t}
		if src == nil {
			msg.err = errors.New("no daemon connection configured")
			return msg
		}
		ctx := context.Background()
		switch t {
		case TabDiff:
			msg.text, msg.err = src.Diff(ctx, id)
		case TabLogs:
			msg.text, msg.err = src.Logs(ctx, id)
		default:
			msg.text, msg.err = src.File(ctx, id, tabFile[t])
		}
		return msg
	}
}

func (m Model) detailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	d := m.det
	page := m.pageSize()
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "esc", "backspace", "left", "h":
		m.det = nil
		return m, nil
	case "r":
		return m, m.loadTab(d.tab)
	case "tab", "right", "l":
		return m.goTo(d.tab + 1)
	case "shift+tab":
		return m.goTo(d.tab - 1)
	case "1", "2", "3", "4", "5":
		return m.goTo(Tab(msg.String()[0] - '1'))
	case "down", "j":
		d.scrollBy(1)
	case "up", "k":
		d.scrollBy(-1)
	case "pgdown", " ", "ctrl+f":
		d.scrollBy(page)
	case "pgup", "ctrl+b":
		d.scrollBy(-page)
	case "ctrl+d":
		d.scrollBy(page / 2)
	case "ctrl+u":
		d.scrollBy(-page / 2)
	case "home", "g":
		d.scrollTo(0)
	case "end", "G":
		d.bottom()
	}
	return m, nil
}

// pageSize is how many lines of a tab fit on screen.
func (m Model) pageSize() int {
	h := m.height
	if h <= 0 {
		h = 24
	}
	return max(h-3, 1) // minus the header, the status bar and the tab bar
}

func (m Model) goTo(t Tab) (tea.Model, tea.Cmd) {
	if m.det.switchTo(t) {
		return m, m.loadTab(m.det.tab)
	}
	return m, nil
}

// Keys documents the keymap; the help overlay and the status bar render from it.
var Keys = []struct{ Key, Help string }{
	{"?", "show or hide this help"},
	{"j/k ↑/↓", "move the selection"},
	{"g/G", "first / last ticket"},
	{"enter", "open the selected ticket"},
	{"esc", "back from a ticket"},
	{"tab/1-5", "next tab / jump to a tab (plan, tasks, contract, diff, logs)"},
	{"pgup/pgdn", "scroll a page; g/G top/bottom"},
	{"r", "refresh now"},
	{"q", "quit"},
	{"ctrl+c", "quit from anywhere"},
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
	case m.det != nil:
		body = m.det.view(w, bodyH)
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
	case m.det != nil:
		s = "esc back · tab switch · j/k scroll · r reload · ? help"
	default:
		s = fmt.Sprintf("connected · %d tickets · enter open · ? help · q quit", len(m.tickets))
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
