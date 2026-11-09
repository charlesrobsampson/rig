package supervisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/supervisor"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var (
	ctx   = context.Background()
	epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

type counting struct {
	*testkit.FakeTerminal
	clock *testkit.FakeClock
	mu    sync.Mutex
	times []time.Time
}

func (c *counting) List(ctx context.Context) ([]terminal.Pane, error) {
	c.mu.Lock()
	c.times = append(c.times, c.clock.Now())
	c.mu.Unlock()
	return c.FakeTerminal.List(ctx)
}

func (c *counting) polls() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.times...)
}

type fixture struct {
	t      *testing.T
	dir    string
	clock  *testkit.FakeClock
	states *state.Manager
	term   *counting
	bus    *events.Bus
	sub    *events.Subscription
	sup    *supervisor.Supervisor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := testkit.NewFakeClock(epoch)
	f := &fixture{t: t, dir: testkit.RealTempDir(t), clock: clock, states: state.NewManager(clock), bus: events.NewBus(clock, 0)}
	f.term = &counting{FakeTerminal: testkit.NewFakeTerminal(), clock: clock}
	f.sub = f.bus.Subscribe()
	<-f.sub.Events()
	t.Cleanup(f.sub.Close)
	f.sup = supervisor.New(supervisor.Options{
		Dir: f.dir, States: f.states, Backend: f.term, Bus: f.bus, Clock: clock,
		Interval: time.Second, HookTTL: time.Minute, MaxBackoff: 8 * time.Second,
	})
	return f
}

// launch makes a ticket whose service has a pane, as a started ticket would.
func (f *fixture) launch(ticket, service string) terminal.PaneID {
	f.t.Helper()
	id, err := f.term.Spawn(ctx, terminal.SpawnSpec{Ticket: ticket, Service: service, Dir: f.dir, Command: []string{"claude"}})
	if err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(f.dir, ticket, "state.json")
	if _, err := f.states.Load(path); err != nil {
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := f.states.Create(path, state.State{TicketID: ticket, Services: map[string]state.ServiceState{}}); err != nil {
			f.t.Fatal(err)
		}
	}
	err = f.states.Update(path, func(s *state.State) error {
		ss := s.Services[service]
		ss.Agent = state.Agent{Backend: "fake", PaneID: string(id), State: domain.AgentUnknown}
		s.Services[service] = ss
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) agentState(ticket, service string) domain.AgentState {
	f.t.Helper()
	st, err := f.states.Load(filepath.Join(f.dir, ticket, "state.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Services[service].Agent.State
}

func (f *fixture) report(ticket, service, st string) error {
	return f.sup.Report(ctx, api.AgentEventParams{Ticket: ticket, Service: service, State: st})
}

func (f *fixture) mustReport(ticket, service, st string) {
	f.t.Helper()
	if err := f.report(ticket, service, st); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) poll() {
	f.t.Helper()
	if err := f.sup.Poll(ctx); err != nil {
		f.t.Fatal(err)
	}
}

// changes drains the agent.state events published so far.
func (f *fixture) changes() []api.AgentStateData {
	f.t.Helper()
	var out []api.AgentStateData
	for {
		select {
		case ev := <-f.sub.Events():
			if ev.Type != api.EventAgentState {
				continue
			}
			var d api.AgentStateData
			if err := jsonUnmarshal(ev.Data, &d); err != nil {
				f.t.Fatal(err)
			}
			out = append(out, d)
		default:
			return out
		}
	}
}

func TestHookReportsSetTheStateAndAnnounceIt(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	tests := []struct {
		report string
		want   domain.AgentState
	}{
		{"ready", domain.AgentIdle}, {"working", domain.AgentWorking}, {"blocked", domain.AgentBlocked},
		{"idle", domain.AgentIdle}, {"exited", domain.AgentExited},
	}
	prev := domain.AgentUnknown
	for _, tt := range tests {
		f.mustReport("demo", "api", tt.report)
		if got := f.agentState("demo", "api"); got != tt.want {
			t.Errorf("after %s: state = %s, want %s", tt.report, got, tt.want)
		}
		ch := f.changes()
		if len(ch) != 1 || ch[0].State != tt.want || ch[0].Previous != prev {
			t.Errorf("after %s: events = %+v, want one %s (was %s)", tt.report, ch, tt.want, prev)
		}
		prev = tt.want
	}
}

func TestAnUnchangedStatePublishesNothing(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	f.mustReport("demo", "api", "working")
	f.changes()
	f.mustReport("demo", "api", "working")
	f.poll()
	f.poll()
	if ch := f.changes(); len(ch) != 0 {
		t.Errorf("events = %+v, want none", ch)
	}
}

func TestBadReportsAreRefused(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	tests := []struct {
		name, ticket, service, state string
		code                         api.ErrorCode
	}{
		{"unknown ticket", "nope", "api", "idle", api.CodeNotFound},
		{"unknown service", "demo", "web", "idle", api.CodeNotFound},
		{"unknown state", "demo", "api", "sleeping", api.CodeInvalid},
		{"empty state", "demo", "api", "", api.CodeInvalid},
		{"invalid ticket id", "../x", "api", "idle", api.CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := f.report(tt.ticket, tt.service, tt.state)
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
	if ch := f.changes(); len(ch) != 0 || f.agentState("demo", "api") != domain.AgentUnknown {
		t.Error("a refused report changed something")
	}
}

func TestAFreshHookReportBeatsTheScreenGuess(t *testing.T) {
	f := newFixture(t)
	pane := f.launch("demo", "api")
	f.mustReport("demo", "api", "blocked")
	f.term.SetHeuristic(pane, domain.AgentIdle)
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentBlocked {
		t.Errorf("state = %s, want blocked (the hook is fresh)", got)
	}

	f.clock.Advance(2 * time.Minute) // past the hook's freshness
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentIdle {
		t.Errorf("state = %s, want idle (the hook is stale and the screen says idle)", got)
	}
	f.term.SetHeuristic(pane, domain.AgentUnknown)
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentBlocked {
		t.Errorf("state = %s; with no guess the last hook report should stand, not unknown", got)
	}
}

func TestTheScreenGuessFillsInWithoutHooks(t *testing.T) {
	f := newFixture(t)
	pane := f.launch("demo", "api")
	f.term.SetHeuristic(pane, domain.AgentWorking)
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentWorking {
		t.Errorf("state = %s, want working", got)
	}
	if ch := f.changes(); len(ch) != 1 || ch[0].Previous != domain.AgentUnknown {
		t.Errorf("events = %+v", ch)
	}
}

func TestADeadPaneMeansExited(t *testing.T) {
	tests := []struct {
		name string
		kill func(*testkit.FakeTerminal, terminal.PaneID)
	}{
		{"process ended, pane kept (tmux)", func(f *testkit.FakeTerminal, id terminal.PaneID) { f.Exit(id, 1) }},
		{"pane vanished (Herdr)", func(f *testkit.FakeTerminal, id terminal.PaneID) { f.Vanish(id) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			pane := f.launch("demo", "api")
			f.mustReport("demo", "api", "working")
			f.changes()
			tt.kill(f.term.FakeTerminal, pane)
			f.poll()
			if got := f.agentState("demo", "api"); got != domain.AgentExited {
				t.Errorf("state = %s, want exited even though the last hook said working", got)
			}
			if ch := f.changes(); len(ch) != 1 || ch[0].State != domain.AgentExited || ch[0].Previous != domain.AgentWorking {
				t.Errorf("events = %+v", ch)
			}
			// a late hook from the dead agent must not revive it
			f.mustReport("demo", "api", "working")
			if got := f.agentState("demo", "api"); got != domain.AgentExited {
				t.Errorf("a report from a dead pane set the state to %s", got)
			}
		})
	}
}

func TestAfterARestartTheRecordedStateStands(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	f.mustReport("demo", "api", "blocked")

	restarted := supervisor.New(supervisor.Options{Dir: f.dir, States: f.states, Backend: f.term, Bus: f.bus, Clock: f.clock})
	if err := restarted.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.agentState("demo", "api"); got != domain.AgentBlocked {
		t.Errorf("state = %s; a restarted daemon that knows nothing new must keep blocked", got)
	}

	// An exited record on a pane that is alive again (the agent was relaunched).
	path := filepath.Join(f.dir, "demo", "state.json")
	f.states.Update(path, func(s *state.State) error {
		ss := s.Services["api"]
		ss.Agent.State = domain.AgentExited
		s.Services["api"] = ss
		return nil
	})
	if err := restarted.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.agentState("demo", "api"); got != domain.AgentUnknown {
		t.Errorf("state = %s, want unknown for a live pane recorded as exited", got)
	}
}

func TestReadyIsRememberedUntilTheAgentExits(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	if f.sup.Ready("demo", "api") {
		t.Error("ready before any report")
	}
	f.mustReport("demo", "api", "working")
	if f.sup.Ready("demo", "api") {
		t.Error("working alone does not say the interface is up")
	}
	f.mustReport("demo", "api", "ready")
	f.mustReport("demo", "api", "working")
	if !f.sup.Ready("demo", "api") {
		t.Error("not ready after a ready report")
	}
	f.mustReport("demo", "api", "exited")
	if f.sup.Ready("demo", "api") {
		t.Error("ready after exit")
	}
}

func TestTicketsAreIndependentAndFinishedOnesIgnored(t *testing.T) {
	f := newFixture(t)
	a := f.launch("one", "api")
	f.launch("two", "api")
	f.term.SetHeuristic(a, domain.AgentWorking)
	f.poll()
	if f.agentState("one", "api") != domain.AgentWorking || f.agentState("two", "api") != domain.AgentUnknown {
		t.Error("one ticket's pane changed the other's state")
	}

	path := filepath.Join(f.dir, "two", "state.json")
	f.states.Update(path, func(s *state.State) error { s.Phase = domain.PhaseDone; return nil })
	f.term.Exit(terminal.PaneID(mustPane(t, f, "two")), 0)
	f.poll()
	if got := f.agentState("two", "api"); got != domain.AgentUnknown {
		t.Errorf("a done ticket was updated to %s", got)
	}
}

func mustPane(t *testing.T, f *fixture, ticket string) string {
	t.Helper()
	st, err := f.states.Load(filepath.Join(f.dir, ticket, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st.Services["api"].Agent.PaneID
}

func TestTheBackendIsNotAskedWhenNothingIsLaunched(t *testing.T) {
	f := newFixture(t)
	f.term.Down(errors.New("herdr is not running"))
	f.poll() // no tickets directory content at all
	if n := len(f.term.polls()); n != 0 {
		t.Errorf("backend asked %d times with no agents to watch", n)
	}
	if err := supervisor.New(supervisor.Options{Dir: filepath.Join(f.dir, "missing"), States: f.states, Backend: f.term, Bus: f.bus, Clock: f.clock}).Poll(ctx); err != nil {
		t.Errorf("a missing tickets directory should not be an error: %v", err)
	}
}

// step advances the fake clock in small steps until the backend has been polled n
// times, giving the polling goroutine a moment to re-arm its timer after each poll.
func (f *fixture) stepUntilPolls(n int) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.term.polls()) < n {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for poll %d (have %d)", n, len(f.term.polls()))
		}
		f.clock.Advance(100 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
}

func TestRunPollsBacksOffWhenTheBackendFailsAndRecovers(t *testing.T) {
	f := newFixture(t)
	pane := f.launch("demo", "api")
	f.term.SetHeuristic(pane, domain.AgentWorking)
	f.term.Down(errors.New("backend down"))

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.sup.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	f.stepUntilPolls(5)
	f.term.Up()
	f.stepUntilPolls(7)

	times := f.term.polls()
	// Failures double the wait up to the cap; the first success returns to the interval.
	wantGaps := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 1 * time.Second}
	for i, want := range wantGaps {
		if gap := times[i+1].Sub(times[i]); gap < want || gap > want+900*time.Millisecond {
			t.Errorf("gap %d = %s, want about %s", i, gap, want)
		}
	}
	if got := f.agentState("demo", "api"); got != domain.AgentWorking {
		t.Errorf("state after recovery = %s, want working", got)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	f := newFixture(t)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.sup.Run(runCtx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}

// eventsFile is where a container agent's hooks write.
func (f *fixture) eventsFile(ticket, service string) string {
	f.t.Helper()
	path := supervisor.EventsFile(f.dir, ticket, service)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	file.WriteString(text)
}

func TestAContainerAgentsHookEventsComeFromItsEventsFile(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")

	appendTo(t, file, "ready\nworking\n")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentWorking {
		t.Errorf("state = %s, want working (the last line)", got)
	}
	if ch := f.changes(); len(ch) != 1 || ch[0].State != domain.AgentWorking {
		t.Errorf("first sight of a file must not replay its history: %+v", ch)
	}

	appendTo(t, file, "blocked\n")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentBlocked {
		t.Errorf("state = %s, want blocked", got)
	}
	appendTo(t, file, "idle\nworking\nblocked\n")
	f.poll()
	ch := f.changes()
	var states []domain.AgentState
	for _, c := range ch {
		states = append(states, c.State)
	}
	if len(states) < 2 || states[len(states)-1] != domain.AgentBlocked {
		t.Errorf("events = %v, want the new lines applied in order, ending blocked", states)
	}

	f.poll()
	f.poll()
	if more := f.changes(); len(more) != 0 {
		t.Errorf("lines were processed again: %+v", more)
	}
}

func TestAPartialLineWaitsForItsNewline(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")
	appendTo(t, file, "idle\nwork")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentIdle {
		t.Errorf("state = %s, want idle (the half-written word must wait)", got)
	}
	appendTo(t, file, "ing\n")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentWorking {
		t.Errorf("state = %s, want working once the line is complete", got)
	}
}

func TestGarbageAndATruncatedEventsFile(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")
	appendTo(t, file, "working\n")
	f.poll()
	appendTo(t, file, "rm -rf /\n\nsleeping\n$(id)\n")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentWorking {
		t.Errorf("state = %s; lines that are not states must be ignored", got)
	}

	os.WriteFile(file, []byte("idle\n"), 0o600) // a new agent run starts a fresh file
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentIdle {
		t.Errorf("state = %s, want idle after the file was truncated", got)
	}
}

func TestAMissingEventsFileIsFine(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentUnknown {
		t.Errorf("state = %s", got)
	}
}
