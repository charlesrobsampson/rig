package contractwatch_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/contract"
	"github.com/charlesrobsampson/rig/internal/contractwatch"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t      *testing.T
	dir    string
	clock  *testkit.FakeClock
	states *state.Manager
	bus    *events.Bus
	sub    *events.Subscription
	w      *contractwatch.Watcher
	writes int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := testkit.NewFakeClock(epoch)
	f := &fixture{t: t, dir: testkit.RealTempDir(t), clock: clock, states: state.NewManager(clock), bus: events.NewBus(clock, 0)}
	f.sub = f.bus.Subscribe()
	<-f.sub.Events()
	t.Cleanup(f.sub.Close)
	f.w = f.newWatcher()
	return f
}

func (f *fixture) newWatcher() *contractwatch.Watcher {
	return contractwatch.New(contractwatch.Options{Dir: f.dir, States: f.states, Bus: f.bus, Clock: f.clock, Interval: 100 * time.Millisecond, Debounce: 500 * time.Millisecond})
}

// ticket creates ticket id in the given phase with the template contract.
func (f *fixture) ticket(id string, phase domain.Phase) {
	f.t.Helper()
	os.MkdirAll(filepath.Join(f.dir, id), 0o700)
	st := state.State{TicketID: id, Phase: phase, Services: map[string]state.ServiceState{"api": {}, "web": {}}}
	st.Contract.AgreedHash = contract.Parse(contract.Template(id)).AgreedHash()
	if err := f.states.Create(filepath.Join(f.dir, id, "state.json"), st); err != nil {
		f.t.Fatal(err)
	}
	f.write(id, contract.Template(id))
}

// write replaces the contract and gives it a new modification time, as an editor would.
func (f *fixture) write(id, content string) {
	f.t.Helper()
	path := filepath.Join(f.dir, id, "contract.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.writes++
	mod := epoch.Add(time.Duration(f.writes) * time.Second)
	os.Chtimes(path, mod, mod)
}

func (f *fixture) contractOf(id string) string {
	raw, _ := os.ReadFile(filepath.Join(f.dir, id, "contract.md"))
	return string(raw)
}

func proposal(service string, n int, status string) string {
	return "### P-" + service + "-" + itoa(n) + " · from " + service + " · " + status + "\n**What:** change " + itoa(n) + "\n**Why:** because\n"
}

func itoa(n int) string { return string(rune('0' + n)) }

// withProposals puts proposals in the Proposed changes section of a template.
func withProposals(id string, entries ...string) string {
	md := contract.Template(id)
	return md + "\n" + "" // keep the template's comments; entries go before the Changelog heading
}

func contractWith(id, agreed string, proposals ...string) string {
	return "# Contract: " + id + "\n\n## Agreed\n" + agreed + "\n\n## Proposed changes\n" + strings.Join(proposals, "\n") + "\n## Changelog\n"
}

func (f *fixture) state(id string) state.State {
	f.t.Helper()
	st, err := f.states.Load(filepath.Join(f.dir, id, "state.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

// poll runs the watcher once and advances the clock by the interval.
func (f *fixture) poll(w *contractwatch.Watcher) {
	w.Poll(context.Background())
	f.clock.Advance(100 * time.Millisecond)
}

// settle polls until the debounce has certainly passed.
func (f *fixture) settle(w *contractwatch.Watcher) {
	for range 8 {
		f.poll(w)
	}
}

type seenEvent struct {
	Type   api.EventType
	Ticket string
	Data   map[string]any
}

func (f *fixture) events() []seenEvent {
	var out []seenEvent
	for {
		select {
		case ev := <-f.sub.Events():
			if strings.HasPrefix(string(ev.Type), "contract.") {
				var data map[string]any
				json.Unmarshal(ev.Data, &data)
				out = append(out, seenEvent{ev.Type, ev.Ticket, data})
			}
		default:
			return out
		}
	}
}

func TestANewProposalSetsTheFlagAndIsAnnouncedOnce(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.settle(f.w)
	if ev := f.events(); len(ev) != 0 || f.state("demo").Contract.Pending {
		t.Fatalf("the untouched template announced %+v", ev)
	}

	f.write("demo", contractWith("demo", "", proposal("api", 1, "open")))
	f.settle(f.w)
	ev := f.events()
	if len(ev) != 1 || ev[0].Type != api.EventContractPending || ev[0].Ticket != "demo" || ev[0].Data["proposal"] != "P-api-1" {
		t.Fatalf("events = %+v", ev)
	}
	st := f.state("demo")
	if !st.Contract.Pending || len(st.Contract.Seen) != 1 || st.Contract.Seen[0] != "P-api-1" {
		t.Errorf("contract state = %+v", st.Contract)
	}

	f.settle(f.w)
	f.settle(f.w)
	if more := f.events(); len(more) != 0 {
		t.Errorf("announced again: %+v", more)
	}

	// a second proposal is a second announcement; a restarted daemon repeats nothing
	f.write("demo", contractWith("demo", "", proposal("api", 1, "open"), proposal("web", 1, "open")))
	f.settle(f.w)
	if ev := f.events(); len(ev) != 1 || ev[0].Data["proposal"] != "P-web-1" {
		t.Errorf("events after the second proposal = %+v", ev)
	}
	restarted := f.newWatcher()
	f.settle(restarted)
	if again := f.events(); len(again) != 0 {
		t.Errorf("a restarted watcher re-announced %+v", again)
	}
}

func TestRapidWritesAreOneEvent(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.settle(f.w)

	// an editor saving every 100 ms for half a second, adding a line each time
	for i := 1; i <= 5; i++ {
		f.write("demo", contractWith("demo", "", proposal("api", 1, "open"), strings.Repeat("note\n", i)))
		f.poll(f.w)
	}
	if ev := f.events(); len(ev) != 0 || f.state("demo").Contract.Pending {
		t.Fatalf("read while the file was still changing: %+v", ev)
	}
	f.settle(f.w)
	if ev := f.events(); len(ev) != 1 || ev[0].Data["proposal"] != "P-api-1" {
		t.Errorf("events after the writes stopped = %+v, want exactly one", ev)
	}
}

func TestEditingTheAgreedSectionAfterThePlanIsApprovedIsAViolation(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.settle(f.w)

	f.write("demo", contractWith("demo", "GET /secretly-changed"))
	f.settle(f.w)
	ev := f.events()
	if len(ev) != 1 || ev[0].Type != api.EventContractViolation || !strings.Contains(ev[0].Data["detail"].(string), "agreed section") {
		t.Fatalf("events = %+v", ev)
	}
	if st := f.state("demo"); st.Contract.Violation == "" {
		t.Error("the violation is not recorded in state.json")
	}

	// editing something else is not a violation, and the same edit is not reported twice
	f.write("demo", contractWith("demo", "GET /secretly-changed", proposal("api", 1, "open")))
	f.settle(f.w)
	if ev := f.events(); len(ev) != 1 || ev[0].Type != api.EventContractPending {
		t.Errorf("events = %+v, want only the proposal announcement", ev)
	}
}

func TestTheAgreedSectionMayChangeWhileThePlanIsBeingDrafted(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhasePlan)
	f.settle(f.w)
	f.write("demo", contractWith("demo", "```endpoint\nGET /x\n```"))
	f.settle(f.w)
	if ev := f.events(); len(ev) != 0 || f.state("demo").Contract.Violation != "" {
		t.Errorf("drafting the contract in the plan phase raised %+v", ev)
	}

	// once the plan moves on, the drafted text is the baseline
	f.states.Update(filepath.Join(f.dir, "demo", "state.json"), func(s *state.State) error { s.Phase = domain.PhaseImplement; return nil })
	f.write("demo", f.contractOf("demo")+"\n<!-- a comment is not content -->\n")
	f.settle(f.w)
	if ev := f.events(); len(ev) != 0 {
		t.Errorf("a comment after the plan raised %+v", ev)
	}
	f.write("demo", contractWith("demo", "```endpoint\nGET /y\n```"))
	f.settle(f.w)
	if ev := f.events(); len(ev) != 1 || ev[0].Type != api.EventContractViolation {
		t.Errorf("events = %+v", ev)
	}
}

func TestTheFlagClearsWhenNoProposalRemainsOpen(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.write("demo", contractWith("demo", "", proposal("api", 1, "open"), proposal("web", 1, "open")))
	f.settle(f.w)
	f.events()
	if !f.state("demo").Contract.Pending {
		t.Fatal("not pending")
	}

	f.write("demo", contractWith("demo", "", proposal("api", 1, "accepted"), proposal("web", 1, "open")))
	f.settle(f.w)
	ev := f.events()
	if len(ev) != 1 || ev[0].Type != api.EventContractResolved || ev[0].Data["proposal"] != "P-api-1" || ev[0].Data["outcome"] != "accepted" {
		t.Fatalf("events = %+v", ev)
	}
	if !f.state("demo").Contract.Pending {
		t.Error("the flag cleared while a proposal is still open")
	}

	rejected := "### P-web-1 · from web · rejected\n**What:** change 1\n**Why:** because\n**Rejected:** not now\n"
	f.write("demo", contractWith("demo", "", proposal("api", 1, "accepted"), rejected))
	f.settle(f.w)
	ev = f.events()
	if len(ev) != 1 || ev[0].Data["outcome"] != "rejected" || f.state("demo").Contract.Pending {
		t.Errorf("events = %+v, pending = %v", ev, f.state("demo").Contract.Pending)
	}
}

func TestProposalsAlreadyInTheFileWhenTheDaemonStartsAreAnnouncedOnceAndKnownOnesAreNot(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.ticket("old", domain.PhaseImplement)
	f.write("demo", contractWith("demo", "", proposal("api", 1, "open")))
	f.write("old", contractWith("old", "", proposal("api", 1, "open")))
	f.states.Update(filepath.Join(f.dir, "old", "state.json"), func(s *state.State) error {
		s.Contract.Seen, s.Contract.Pending = []string{"P-api-1"}, true
		return nil
	})

	f.settle(f.w)
	ev := f.events()
	if len(ev) != 1 || ev[0].Ticket != "demo" || ev[0].Data["proposal"] != "P-api-1" {
		t.Errorf("events = %+v; the proposal that arrived while the daemon was down is announced, the already-announced one is not", ev)
	}
}

func TestUnreadableDeletedAndMalformedContractsAreSurvived(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.write("demo", "this is not a contract at all")
	f.settle(f.w)
	if ev := f.events(); len(ev) != 0 {
		t.Errorf("garbage raised %+v", ev)
	}
	os.Remove(filepath.Join(f.dir, "demo", "contract.md"))
	f.settle(f.w)
	f.write("demo", contractWith("demo", "", proposal("api", 1, "open")))
	f.settle(f.w)
	if ev := f.events(); len(ev) != 1 {
		t.Errorf("a contract that came back = %+v", ev)
	}

	// a ticket without a contract (a single service) is simply ignored
	os.MkdirAll(filepath.Join(f.dir, "solo"), 0o700)
	f.settle(f.w)
}

func TestRunPollsOnTheClockAndStopsWhenCancelled(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.write("demo", contractWith("demo", "", proposal("api", 1, "open")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.w.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for !f.state("demo").Contract.Pending {
		if time.Now().After(deadline) {
			t.Fatal("the running watcher never noticed the proposal")
		}
		f.clock.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestTheTemplateAlonePutsNothingPending(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", domain.PhaseImplement)
	f.settle(f.w)
	st := f.state("demo")
	if st.Contract.Pending || st.Contract.Violation != "" || len(st.Contract.Seen) != 0 {
		t.Errorf("state = %+v", st.Contract)
	}
	_ = withProposals
}
