package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

type fakeSource struct {
	tickets []api.TicketSummary
	err     error
	calls   int
	// files, diff and logs are what the other reads return; a name missing from
	// files is ErrMissing. fileErr, when set, fails every read.
	files   map[string]string
	diff    string
	logs    string
	fileErr error
	reads   []string

	// acts records every action the TUI asked for, as "name args"; actErr fails the
	// action with that name; the rest is what the actions answer.
	acts      []string
	actErr    map[string]error
	proposals []api.ContractProposal
	attachCmd []string
	pushRes   api.PushRunResult
	stopErr   error // returned by a stop that is not forced
}

func (f *fakeSource) act(name string, args ...any) error {
	f.acts = append(f.acts, strings.TrimSpace(name+" "+fmt.Sprint(args...)))
	return f.actErr[name]
}

func (f *fakeSource) Approve(_ context.Context, ticket string, gate domain.Gate) error {
	return f.act("approve", ticket, " ", gate)
}

func (f *fakeSource) MovePhase(_ context.Context, ticket string, to domain.Phase) error {
	return f.act("phase", ticket, " ", to)
}

func (f *fakeSource) Push(_ context.Context, ticket string) (api.PushRunResult, error) {
	return f.pushRes, f.act("push", ticket)
}

func (f *fakeSource) Stop(_ context.Context, ticket string, force bool) (api.TicketStopResult, error) {
	if !force && f.stopErr != nil {
		f.act("stop", ticket, " force=false")
		return api.TicketStopResult{}, f.stopErr
	}
	return api.TicketStopResult{}, f.act("stop", ticket, fmt.Sprintf(" force=%v", force))
}

func (f *fakeSource) Message(_ context.Context, ticket, service, text string) error {
	return f.act("message", ticket, "/", service, ": ", text)
}

func (f *fakeSource) Proposals(context.Context, string) ([]api.ContractProposal, error) {
	return f.proposals, f.actErr["proposals"]
}

func (f *fakeSource) Resolve(_ context.Context, ticket, proposal string, accept bool, reason string) error {
	return f.act("resolve", ticket, " ", proposal, fmt.Sprintf(" accept=%v reason=%q", accept, reason))
}

func (f *fakeSource) AttachCommand(_ context.Context, ticket, service string) ([]string, error) {
	return f.attachCmd, f.act("attach", ticket, "/", service)
}

func (f *fakeSource) File(_ context.Context, ticket, name string) (string, error) {
	f.reads = append(f.reads, ticket+"/"+name)
	if f.fileErr != nil {
		return "", f.fileErr
	}
	text, ok := f.files[name]
	if !ok {
		return "", ErrMissing
	}
	return text, nil
}

func (f *fakeSource) Diff(_ context.Context, ticket string) (string, error) {
	f.reads = append(f.reads, ticket+"/diff")
	if f.diff == "" {
		return "", ErrMissing
	}
	return f.diff, nil
}

func (f *fakeSource) Logs(_ context.Context, ticket string) (string, error) {
	f.reads = append(f.reads, ticket+"/logs")
	return f.logs, f.fileErr
}

func (f *fakeSource) Tickets(context.Context) ([]api.TicketSummary, error) {
	f.calls++
	return f.tickets, f.err
}

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// send applies a message and returns the new model and the command it asked for.
func send(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

// load runs the model's Init command and feeds its result back.
func load(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
	return send(m, cmd())
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

var demo = []api.TicketSummary{{ID: "demo-1", Title: "Add locale", Phase: domain.PhaseImplement}}

func TestTheFleetShowsOnceTheDaemonAnswers(t *testing.T) {
	src := &fakeSource{tickets: demo}
	m := New(Options{Source: src, Version: "v1"})
	if v := m.View(); !strings.Contains(v, "connecting") {
		t.Errorf("before the first answer:\n%s", v)
	}
	m, _ = send(m, tea.WindowSizeMsg{Width: 80, Height: 20})
	m, _ = load(t, m)
	v := m.View()
	for _, want := range []string{"rig v1", "demo-1", "implement", "Add locale", "connected · 1 tickets"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestAnEmptyFleetSaysHowToStart(t *testing.T) {
	m, _ := load(t, New(Options{Source: &fakeSource{}}))
	if v := m.View(); !strings.Contains(v, "no tickets yet") || !strings.Contains(v, "rig new") {
		t.Errorf("view:\n%s", v)
	}
}

func TestHelpTogglesAndSwallowsOtherKeys(t *testing.T) {
	m, _ := load(t, New(Options{Source: &fakeSource{tickets: demo}}))
	m, _ = send(m, key("?"))
	if v := m.View(); !strings.Contains(v, "Keys") || !strings.Contains(v, "refresh now") || strings.Contains(v, "demo-1") {
		t.Errorf("help view:\n%s", v)
	}
	// q does not quit while the overlay is open; it only closes with ? or esc
	if m2, cmd := send(m, key("q")); isQuit(cmd) || !m2.help {
		t.Error("q must not quit from inside the help overlay")
	}
	m, _ = send(m, key("?"))
	if strings.Contains(m.View(), "Keys") {
		t.Error("? did not close the help")
	}
	m, _ = send(m, key("?"))
	m, _ = send(m, key("esc"))
	if m.help {
		t.Error("esc did not close the help")
	}
}

func TestQuit(t *testing.T) {
	m, _ := load(t, New(Options{Source: &fakeSource{}}))
	for _, k := range []string{"q", "ctrl+c"} {
		if _, cmd := send(m, key(k)); !isQuit(cmd) {
			t.Errorf("%s did not quit", k)
		}
	}
	// ctrl+c quits even from the help overlay
	h, _ := send(m, key("?"))
	if _, cmd := send(h, key("ctrl+c")); !isQuit(cmd) {
		t.Error("ctrl+c must always quit")
	}
	quit, _ := send(m, key("q"))
	if quit.View() != "" {
		t.Errorf("a quitting model should draw nothing, drew %q", quit.View())
	}
}

func TestADaemonThatIsNotRunningShowsItAndKeepsLooking(t *testing.T) {
	src := &fakeSource{err: errors.New("dial unix /x/rig.sock: connect: no such file or directory")}
	m := New(Options{Source: src, RetryEvery: 5 * time.Second})
	m, cmd := load(t, m)
	v := m.View()
	for _, want := range []string{"daemon is not running", "no such file", "every 5s", "daemon not running · r retry"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if cmd == nil {
		t.Fatal("no retry was scheduled")
	}

	// a second failure while the timer is pending must not stack another timer
	m, cmd2 := send(m, loadedMsg{err: errors.New("still down")})
	if cmd2 != nil {
		t.Error("a second retry timer was scheduled")
	}
	// the timer fires: ask again
	m, ask := send(m, retryMsg{})
	if ask == nil {
		t.Fatal("the retry did not look again")
	}
	if src.calls != 1 {
		t.Fatalf("calls = %d", src.calls)
	}
	ask()
	if src.calls != 2 {
		t.Errorf("calls = %d after the retry", src.calls)
	}
}

func TestReconnectingRestoresTheFleetAndStopsRetrying(t *testing.T) {
	src := &fakeSource{err: errors.New("down")}
	m, _ := load(t, New(Options{Source: src}))
	src.err, src.tickets = nil, demo
	m, ask := send(m, retryMsg{})
	m, cmd := send(m, ask())
	if !m.connected || !strings.Contains(m.View(), "demo-1") || strings.Contains(m.View(), "not running") {
		t.Errorf("after reconnecting:\n%s", m.View())
	}
	if cmd != nil {
		t.Error("a connected model scheduled a retry")
	}
	// a stray retry timer after reconnecting does nothing
	if _, cmd := send(m, retryMsg{}); cmd != nil {
		t.Error("a retry fired while connected")
	}
	// and if the daemon goes away again, a new retry starts
	m, cmd = send(m, loadedMsg{err: errors.New("gone")})
	if cmd == nil || !strings.Contains(m.View(), "not running") {
		t.Error("losing the daemon again was not noticed")
	}
}

func TestRRefreshesImmediately(t *testing.T) {
	src := &fakeSource{tickets: demo}
	m, _ := load(t, New(Options{Source: src}))
	src.tickets = append(src.tickets, api.TicketSummary{ID: "demo-2"})
	m, cmd := send(m, key("r"))
	m, _ = send(m, cmd())
	if !strings.Contains(m.View(), "demo-2") {
		t.Errorf("view:\n%s", m.View())
	}
}

func TestTinyAndOddTerminalsDoNotPanic(t *testing.T) {
	long := []api.TicketSummary{{ID: strings.Repeat("x", 100), Title: strings.Repeat("é", 100), Phase: domain.PhasePlan}}
	for _, size := range [][2]int{{0, 0}, {1, 1}, {5, 2}, {10, 3}, {20, 1}, {300, 100}} {
		for _, src := range []*fakeSource{{tickets: long}, {err: errors.New(strings.Repeat("e", 200))}, {}} {
			m, _ := load(t, New(Options{Source: src, Version: "dev"}))
			m, _ = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			for _, k := range []string{"?", "?", "r"} {
				m, _ = send(m, key(k))
			}
			v := m.View()
			if size[0] > 0 {
				for _, line := range strings.Split(v, "\n") {
					if w := len([]rune(line)); w > size[0] && size[0] >= 1 {
						t.Errorf("%dx%d: line of width %d: %q", size[0], size[1], w, line)
					}
				}
			}
			if size[1] > 0 && len(strings.Split(v, "\n")) > size[1] {
				t.Errorf("%dx%d: %d lines", size[0], size[1], len(strings.Split(v, "\n")))
			}
		}
	}
}

func TestWithoutASourceTheTUIReportsItself(t *testing.T) {
	m, _ := load(t, New(Options{}))
	if !strings.Contains(m.View(), "no daemon connection configured") {
		t.Errorf("view:\n%s", m.View())
	}
}
