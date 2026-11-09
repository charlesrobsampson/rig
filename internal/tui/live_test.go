package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func event(t *testing.T, typ api.EventType, ticket, service string, data any) api.Event {
	t.Helper()
	ev, err := api.NewEvent(typ, ticket, service, time.Now(), data)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func agentEvent(t *testing.T, ticket, service string, state domain.AgentState) api.Event {
	return event(t, api.EventAgentState, ticket, service, api.AgentStateData{State: state})
}

// liveModel is a connected model on a fleet of two tickets, with the subscription up.
func liveModel(t *testing.T) (Model, *fakeSource) {
	t.Helper()
	src := &fakeSource{}
	m := ready(t, src,
		api.TicketSummary{ID: "alpha", Phase: domain.PhaseImplement, Services: []api.ServiceSummary{{Name: "api", AgentState: domain.AgentIdle}, {Name: "web", AgentState: domain.AgentIdle}}},
		api.TicketSummary{ID: "beta", Phase: domain.PhasePlan, Services: []api.ServiceSummary{{Name: "api", AgentState: domain.AgentWorking}}},
	)
	m, cmd := send(m, subscribedMsg{})
	m, _ = run(t, m, cmd)
	return m, src
}

func tickets(src *fakeSource, t ...api.TicketSummary) { src.tickets = t }

func TestGoingLiveReadsTheFleetAgainToCoverTheGap(t *testing.T) {
	src := &fakeSource{tickets: demo}
	m := New(Options{Source: src})
	m, _ = load(t, m)
	before := src.calls
	src.tickets = append(src.tickets, api.TicketSummary{ID: "new-one"})
	m, cmd := send(m, subscribedMsg{})
	m, _ = run(t, m, cmd)
	if src.calls != before+1 || !strings.Contains(m.View(), "new-one") || !m.live {
		t.Errorf("calls %d→%d, live %v:\n%s", before, src.calls, m.live, m.View())
	}
	if !strings.Contains(m.View(), "connected · live") {
		t.Errorf("the status bar does not say it is live:\n%s", m.View())
	}
}

func TestAnAgentStateEventMovesTheLightWithoutAskingTheDaemon(t *testing.T) {
	m, src := liveModel(t)
	calls := src.calls
	m, cmd := send(m, eventsMsg{[]api.Event{agentEvent(t, "alpha", "api", domain.AgentBlocked)}})
	if cmd != nil {
		t.Errorf("an agent state event asked for more work")
	}
	v := m.View()
	if !strings.Contains(v, "! api") || !strings.Contains(v, "BLOCKED") {
		t.Errorf("the light did not change:\n%s", v)
	}
	if src.calls != calls {
		t.Error("the fleet was re-read for an agent state event")
	}
	// the blocked ticket now sorts to the top and the other lights are unchanged
	if lines := strings.Split(v, "\n"); !strings.Contains(lines[2], "alpha") {
		t.Errorf("blocked ticket is not first:\n%s", v)
	}
	if !strings.Contains(v, "○ web") {
		t.Errorf("another service's light changed:\n%s", v)
	}
}

func TestAPhaseEventUpdatesTheRowAtOnceAndRereadsForTheGate(t *testing.T) {
	m, src := liveModel(t)
	calls := src.calls
	phase := event(t, api.EventPhaseChanged, "beta", "", api.PhaseChangedData{From: domain.PhasePlan, To: domain.PhaseImplement, By: "human"})
	m, cmd := send(m, eventsMsg{[]api.Event{phase}})
	if v := m.View(); !strings.Contains(v, "implement") || strings.Contains(v, " plan ") {
		t.Errorf("the row still shows the old phase:\n%s", v)
	}
	if cmd == nil {
		t.Fatal("no re-read was asked for; the pending gate would go stale")
	}
	tickets(src, api.TicketSummary{ID: "beta", Phase: domain.PhaseImplement, PendingGate: ""}, api.TicketSummary{ID: "alpha"})
	m, _ = run(t, m, cmd)
	if src.calls != calls+1 {
		t.Errorf("calls = %d, want %d", src.calls, calls+1)
	}
}

func TestEventsThatChangeASummaryTriggerOneRead(t *testing.T) {
	for _, typ := range []api.EventType{api.EventTicketCreated, api.EventTicketStarted, api.EventTicketStopped, api.EventServiceStatus,
		api.EventGateApproved, api.EventContractPending, api.EventContractResolved, api.EventContractViolation,
		api.EventReconciled, api.EventSummaryUpdated} {
		m, _ := liveModel(t)
		_, cmd := send(m, eventsMsg{[]api.Event{event(t, typ, "alpha", "", api.EmptyData{})}})
		if cmd == nil {
			t.Errorf("%s did not refresh the fleet", typ)
		}
	}
	for _, typ := range []api.EventType{api.EventEgressDenied, api.EventError, api.EventSubscribed, "something.new"} {
		m, _ := liveModel(t)
		if _, cmd := send(m, eventsMsg{[]api.Event{event(t, typ, "alpha", "", api.EmptyData{})}}); cmd != nil {
			t.Errorf("%s caused work", typ)
		}
	}
}

func TestEventsForUnknownTicketsOrWithBadDataFallBackToARead(t *testing.T) {
	m, _ := liveModel(t)
	if _, cmd := send(m, eventsMsg{[]api.Event{agentEvent(t, "ghost", "api", domain.AgentIdle)}}); cmd == nil {
		t.Error("an event for an unseen ticket did not trigger a read")
	}
	if _, cmd := send(m, eventsMsg{[]api.Event{agentEvent(t, "alpha", "nope", domain.AgentIdle)}}); cmd == nil {
		t.Error("an event for an unseen service did not trigger a read")
	}
	bad := api.Event{Type: api.EventAgentState, Ticket: "alpha", Service: "api", Data: json.RawMessage(`"not an object"`)}
	if _, cmd := send(m, eventsMsg{[]api.Event{bad}}); cmd == nil {
		t.Error("an undecodable event did not trigger a read")
	}
	badPhase := api.Event{Type: api.EventPhaseChanged, Ticket: "alpha", Data: json.RawMessage(`[]`)}
	if _, cmd := send(m, eventsMsg{[]api.Event{badPhase}}); cmd == nil {
		t.Error("an undecodable phase event did not trigger a read")
	}
}

func TestABurstOfEventsCostsOneReadAtATime(t *testing.T) {
	m, src := liveModel(t)
	calls := src.calls
	var first tea.Cmd
	burst := []api.Event{event(t, api.EventContractPending, "alpha", "", api.EmptyData{})}
	m, first = send(m, eventsMsg{burst})
	if first == nil {
		t.Fatal("the first event did not start a read")
	}
	for range 50 { // the read is under way: the rest only mark the fleet dirty
		var cmd tea.Cmd
		m, cmd = send(m, eventsMsg{burst})
		if cmd != nil {
			t.Fatal("an event started a second read while one was in flight")
		}
	}
	m, again := run(t, m, first)
	if src.calls != calls+2 { // the one read, then one more for everything that came in meanwhile
		t.Errorf("reads = %d, want 2", src.calls-calls)
	}
	_ = again
	if m.loading || m.dirty {
		t.Errorf("loading %v, dirty %v after the reads finished", m.loading, m.dirty)
	}
}

func TestAHugeBurstDoesNotBlockRendering(t *testing.T) {
	m, _ := liveModel(t)
	var batch []api.Event
	states := []domain.AgentState{domain.AgentWorking, domain.AgentIdle, domain.AgentBlocked}
	for i := 0; i < 20000; i++ {
		batch = append(batch, agentEvent(t, "alpha", "api", states[i%3]))
	}
	start := time.Now()
	m, _ = send(m, eventsMsg{batch})
	_ = m.View()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a 20000-event burst took %v", d)
	}
	// 19999 % 3 == 1 → idle was the last state
	if got := m.tickets[0].Services[0].AgentState; m.tickets[0].ID == "alpha" && got != domain.AgentIdle {
		t.Errorf("final state = %s, want the last event's", got)
	}
}

func TestADroppedSubscriptionRefreshesAndMarksItNotLive(t *testing.T) {
	m, src := liveModel(t)
	calls := src.calls
	src.tickets = append(src.tickets, api.TicketSummary{ID: "missed-while-away"})
	m, cmd := send(m, droppedMsg{})
	if m.live {
		t.Error("still live after the drop")
	}
	m, _ = run(t, m, cmd)
	if src.calls != calls+1 || !strings.Contains(m.View(), "missed-while-away") {
		t.Errorf("the fleet was not refreshed after the drop:\n%s", m.View())
	}
	if strings.Contains(m.View(), "· live") {
		t.Errorf("the status bar still claims live:\n%s", m.View())
	}
	// coming back refreshes again and is live
	m, cmd = send(m, subscribedMsg{})
	m, _ = run(t, m, cmd)
	if !m.live || !strings.Contains(m.View(), "· live") {
		t.Errorf("not live after resubscribing:\n%s", m.View())
	}
}

func TestADropWhileTheDaemonIsGoneShowsTheDownStateAndRecovers(t *testing.T) {
	m, src := liveModel(t)
	src.err = errors.New("connection refused")
	m, cmd := send(m, droppedMsg{err: errors.New("connection refused")})
	m, _ = run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "daemon is not running") {
		t.Fatalf("view:\n%s", v)
	}
	src.err = nil
	m, cmd = send(m, subscribedMsg{})
	m, _ = run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "alpha") || strings.Contains(v, "not running") {
		t.Errorf("the fleet did not come back:\n%s", v)
	}
}

func TestEventsForTheOpenTicketReloadItsTab(t *testing.T) {
	m, src := liveModel(t)
	src.files = map[string]string{"contract.md": "# Contract v1\n"}
	m = press(t, m, "g", "enter", "3")
	if !strings.Contains(m.View(), "Contract v1") {
		t.Fatalf("view:\n%s", m.View())
	}
	src.files["contract.md"] = "# Contract v2\n"
	m, cmd := send(m, eventsMsg{[]api.Event{event(t, api.EventContractPending, "alpha", "", api.EmptyData{})}})
	m, _ = run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "Contract v2") {
		t.Errorf("the open tab did not follow the event:\n%s", v)
	}
	// an event for another ticket leaves the open tab alone
	src.files["contract.md"] = "# Contract v3\n"
	m, cmd = send(m, eventsMsg{[]api.Event{event(t, api.EventContractPending, "beta", "", api.EmptyData{})}})
	m, _ = run(t, m, cmd)
	if v := m.View(); strings.Contains(v, "Contract v3") {
		t.Errorf("another ticket's event reloaded this tab:\n%s", v)
	}
}

// ---- the pump ----

type sink struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (s *sink) send(m tea.Msg) { s.mu.Lock(); s.msgs = append(s.msgs, m); s.mu.Unlock() }

func (s *sink) snapshot() []tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tea.Msg(nil), s.msgs...)
}

func (s *sink) waitFor(t *testing.T, what string, ok func([]tea.Msg) bool) []tea.Msg {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.snapshot(); ok(got) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; got %d messages: %v", what, len(s.snapshot()), s.snapshot())
	return nil
}

func countOf[T any](msgs []tea.Msg) int {
	n := 0
	for _, m := range msgs {
		if _, ok := m.(T); ok {
			n++
		}
	}
	return n
}

// scriptedSub is a Subscriber whose subscriptions the test controls.
type scriptedSub struct {
	mu    sync.Mutex
	chans []chan api.Event
	errs  []error // consumed one per call; nil means success
	calls []time.Time
}

func (s *scriptedSub) Subscribe(context.Context) (<-chan api.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, time.Now())
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	ch := make(chan api.Event, 4096)
	s.chans = append(s.chans, ch)
	return ch, nil
}

func (s *scriptedSub) chan_(i int) chan api.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < len(s.chans) {
		return s.chans[i]
	}
	return nil
}

func (s *scriptedSub) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }

func startPump(t *testing.T, sub Subscriber, retry time.Duration) (*sink, context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := &sink{}
	done := make(chan struct{})
	go func() { Pump(ctx, sub, out.send, retry); close(done) }()
	return out, cancel, done
}

func TestThePumpAnnouncesTheSubscriptionAndDeliversEventsInBatches(t *testing.T) {
	sub := &scriptedSub{}
	out, _, _ := startPump(t, sub, 10*time.Millisecond)
	out.waitFor(t, "subscribed", func(m []tea.Msg) bool { return countOf[subscribedMsg](m) == 1 })
	ch := sub.chan_(0)
	for i := 0; i < 1000; i++ {
		ch <- api.Event{Type: api.EventAgentState, Ticket: "t"}
	}
	msgs := out.waitFor(t, "1000 events", func(m []tea.Msg) bool {
		n := 0
		for _, x := range m {
			if e, ok := x.(eventsMsg); ok {
				n += len(e.events)
			}
		}
		return n == 1000
	})
	batches := countOf[eventsMsg](msgs)
	if batches < 4 || batches > 1000 {
		t.Errorf("%d batches for 1000 queued events; they should be grouped (at most %d each)", batches, maxBatch)
	}
	for _, x := range msgs {
		if e, ok := x.(eventsMsg); ok && len(e.events) > maxBatch {
			t.Errorf("a batch of %d exceeds the limit", len(e.events))
		}
	}
}

func TestThePumpResubscribesWhenTheStreamEnds(t *testing.T) {
	sub := &scriptedSub{}
	out, _, _ := startPump(t, sub, 5*time.Millisecond)
	out.waitFor(t, "first subscription", func(m []tea.Msg) bool { return countOf[subscribedMsg](m) == 1 })
	ch := sub.chan_(0)
	ch <- api.Event{Type: api.EventTicketStarted, Ticket: "t"}
	close(ch)
	msgs := out.waitFor(t, "a drop and a second subscription", func(m []tea.Msg) bool {
		return countOf[droppedMsg](m) == 1 && countOf[subscribedMsg](m) == 2
	})
	// the event queued before the close was still delivered, and before the drop
	var order []string
	for _, m := range msgs {
		switch m.(type) {
		case subscribedMsg:
			order = append(order, "sub")
		case eventsMsg:
			order = append(order, "ev")
		case droppedMsg:
			order = append(order, "drop")
		}
	}
	if strings.Join(order, ",") != "sub,ev,drop,sub" {
		t.Errorf("order = %v", order)
	}
	// events on the new subscription arrive
	sub.chan_(1) <- api.Event{Type: api.EventTicketStopped, Ticket: "t"}
	out.waitFor(t, "an event on the new stream", func(m []tea.Msg) bool { return countOf[eventsMsg](m) == 2 })
}

func TestThePumpRetriesAFailedSubscriptionAndReportsEachFailure(t *testing.T) {
	sub := &scriptedSub{errs: []error{errors.New("refused"), errors.New("refused again"), nil}}
	out, _, _ := startPump(t, sub, 15*time.Millisecond)
	msgs := out.waitFor(t, "success after two failures", func(m []tea.Msg) bool { return countOf[subscribedMsg](m) == 1 })
	var errs []string
	for _, m := range msgs {
		if d, ok := m.(droppedMsg); ok && d.err != nil {
			errs = append(errs, d.err.Error())
		}
	}
	if strings.Join(errs, "|") != "refused|refused again" {
		t.Errorf("failures reported = %v", errs)
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.calls) != 3 || sub.calls[1].Sub(sub.calls[0]) < 10*time.Millisecond {
		t.Errorf("attempts %d; they must be spaced by the retry delay", len(sub.calls))
	}
}

func TestAStreamThatDiesAtOnceDoesNotSpinThePump(t *testing.T) {
	sub := &scriptedSub{}
	// every subscription is closed the moment it is made
	ctx, cancel := context.WithCancel(context.Background())
	out := &sink{}
	done := make(chan struct{})
	closing := &closingSub{inner: sub}
	go func() { Pump(ctx, closing, out.send, 30*time.Millisecond); close(done) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	if n := sub.count(); n > 10 {
		t.Errorf("%d subscriptions in 200ms; the pump is spinning", n)
	}
	if n := sub.count(); n < 2 {
		t.Errorf("only %d subscriptions; it should keep trying", n)
	}
}

type closingSub struct{ inner *scriptedSub }

func (c *closingSub) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	ch, err := c.inner.Subscribe(ctx)
	if err == nil {
		close(c.inner.chan_(c.inner.count() - 1))
	}
	return ch, err
}

func TestThePumpStopsWhenItsContextEnds(t *testing.T) {
	sub := &scriptedSub{}
	out, cancel, done := startPump(t, sub, time.Hour)
	out.waitFor(t, "subscribed", func(m []tea.Msg) bool { return countOf[subscribedMsg](m) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the pump did not stop")
	}
	// ... also while it waits to retry
	failing := &scriptedSub{errs: []error{errors.New("x"), errors.New("x"), errors.New("x")}}
	_, cancel2, done2 := startPump(t, failing, time.Hour)
	time.Sleep(20 * time.Millisecond)
	cancel2()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("the pump did not stop while waiting to retry")
	}
}
