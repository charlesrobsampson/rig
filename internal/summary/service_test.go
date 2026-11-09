package summary_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/store"
	"github.com/charlesrobsampson/rig/internal/summary"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// fakeInputs serves inputs per ticket and counts how often each is gathered.
type fakeInputs struct {
	mu     sync.Mutex
	inputs map[string]summary.Input
	errs   map[string]error
	gather map[string]int
}

func (f *fakeInputs) Gather(_ context.Context, ticket string) (summary.Input, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gather == nil {
		f.gather = map[string]int{}
	}
	f.gather[ticket]++
	if err := f.errs[ticket]; err != nil {
		return summary.Input{}, err
	}
	return f.inputs[ticket], nil
}

func (f *fakeInputs) set(ticket string, in summary.Input) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs == nil {
		f.inputs = map[string]summary.Input{}
	}
	f.inputs[ticket] = in
}

// scripted is a Summarizer that returns canned answers and counts calls.
type scripted struct {
	mu    sync.Mutex
	calls []summary.Input
	text  string
	err   error
	name  string
	delay time.Duration
}

func (s *scripted) Name() string {
	if s.name != "" {
		return s.name
	}
	return "ollama"
}
func (s *scripted) Model() string { return "test-model" }
func (s *scripted) Remote() bool  { return s.name == "claude" }
func (s *scripted) Summarize(ctx context.Context, in summary.Input) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, in)
	text, err, delay := s.text, s.err, s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return text, err
}
func (s *scripted) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }
func (s *scripted) set(text string, err error) {
	s.mu.Lock()
	s.text, s.err = text, err
	s.mu.Unlock()
}

type fixture struct {
	t      *testing.T
	dir    string
	clock  *testkit.FakeClock
	store  *store.FileStore
	inputs *fakeInputs
	sum    *scripted
	bus    *events.Bus
	sub    *events.Subscription
	svc    *summary.Service
}

func newFixture(t *testing.T, tickets ...string) *fixture {
	t.Helper()
	dir := t.TempDir()
	for _, id := range tickets {
		os.MkdirAll(filepath.Join(dir, id), 0o700)
		os.WriteFile(filepath.Join(dir, id, "ticket.yaml"), []byte("id: "+id+"\n"), 0o600)
	}
	st, err := store.OpenFile(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewFakeClock(epoch)
	f := &fixture{t: t, dir: dir, clock: clock, store: st, inputs: &fakeInputs{}, sum: &scripted{text: "- a summary"}, bus: events.NewBus(clock, 0)}
	f.sub = f.bus.Subscribe()
	<-f.sub.Events()
	t.Cleanup(f.sub.Close)
	f.svc = f.build(f.sum)
	return f
}

func (f *fixture) build(s summary.Summarizer) *summary.Service {
	return summary.NewService(summary.Options{
		Dir: f.dir, Store: f.store, Summarizer: s, Inputs: f.inputs, Bus: f.bus, Clock: f.clock,
		MinBackoff: time.Minute, MaxBackoff: 8 * time.Minute, RequestTimeout: 200 * time.Millisecond,
	})
}

func (f *fixture) events() []api.Event {
	var out []api.Event
	for {
		select {
		case ev := <-f.sub.Events():
			if ev.Type == api.EventSummaryUpdated {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

func (f *fixture) poll() { f.svc.Poll(context.Background()) }

func TestASummaryIsMadeStoredAndShownWithItsStats(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	if f.svc.Info("demo") != nil {
		t.Fatal("a summary before the first poll")
	}
	f.poll()
	info := f.svc.Info("demo")
	if info == nil || info.Text != "- a summary" || info.Backend != "ollama" || info.Model != "test-model" || info.Remote {
		t.Fatalf("info = %+v", info)
	}
	if info.Files != 2 || info.Added != 30 || info.Deleted != 4 || info.At == nil || !info.At.Equal(epoch) || info.Error != "" {
		t.Errorf("stats or time wrong: %+v", info)
	}
	if evs := f.events(); len(evs) != 1 || evs[0].Ticket != "demo" {
		t.Errorf("events = %+v", evs)
	}
	// the summary lives in the runtime store, not in the ticket's files
	if entries, _ := os.ReadDir(filepath.Join(f.dir, "demo")); len(entries) != 1 {
		t.Errorf("the ticket directory was written to: %v", entries)
	}
}

func TestUnchangedContentIsNotAskedAboutAgain(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.poll()
	f.poll()
	f.poll()
	if f.sum.count() != 1 {
		t.Errorf("the summarizer was asked %d times for unchanged content", f.sum.count())
	}
	if len(f.events()) != 1 {
		t.Error("an unchanged poll published an event")
	}
}

func TestChangedContentIsSummarizedAgain(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.poll()
	changed := input()
	changed.Tasks += "\n- [x] tests"
	f.inputs.set("demo", changed)
	f.sum.set("- now it is done", nil)
	f.clock.Advance(time.Minute)
	f.poll()
	if f.sum.count() != 2 || f.svc.Info("demo").Text != "- now it is done" {
		t.Errorf("asked %d times; info %+v", f.sum.count(), f.svc.Info("demo"))
	}
	if at := f.svc.Info("demo").At; at == nil || !at.Equal(epoch.Add(time.Minute)) {
		t.Errorf("at = %v", at)
	}
}

func TestSummariesSurviveARestart(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.poll()
	again := f.build(f.sum) // a new daemon on the same store
	again.Poll(context.Background())
	if f.sum.count() != 1 || again.Info("demo") == nil || again.Info("demo").Text != "- a summary" {
		t.Errorf("asked %d times after a restart", f.sum.count())
	}
}

func TestAFailureKeepsThePreviousSummaryAndRecordsTheError(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.poll()
	changed := input()
	changed.Plan = "A different plan."
	f.inputs.set("demo", changed)
	f.sum.set("", errors.New("ollama at localhost:11434: connection refused"))
	f.events()
	f.poll()
	info := f.svc.Info("demo")
	if info == nil || info.Text != "- a summary" || !strings.Contains(info.Error, "connection refused") {
		t.Fatalf("info = %+v; the old text must stay and the error be recorded", info)
	}
	if evs := f.events(); len(evs) != 1 || !strings.Contains(string(evs[0].Data), "connection refused") {
		t.Errorf("events = %+v", evs)
	}

	// success replaces the text and clears the error
	f.sum.set("- recovered", nil)
	f.clock.Advance(time.Minute)
	f.poll()
	if info := f.svc.Info("demo"); info.Text != "- recovered" || info.Error != "" {
		t.Errorf("after recovery: %+v", info)
	}
}

func TestAFailureWithNoPreviousSummaryStillShowsTheError(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.sum.set("", errors.New("no model"))
	f.poll()
	if info := f.svc.Info("demo"); info == nil || info.Text != "" || info.Error != "no model" {
		t.Errorf("info = %+v", info)
	}
}

func TestFailedContentBacksOffAndRetriesWithGrowingDelay(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.sum.set("", errors.New("down"))
	f.poll()
	if f.sum.count() != 1 {
		t.Fatalf("calls = %d", f.sum.count())
	}
	f.clock.Advance(30 * time.Second)
	f.poll() // still inside the first minute
	if f.sum.count() != 1 {
		t.Errorf("retried inside the backoff: %d calls", f.sum.count())
	}
	f.clock.Advance(31 * time.Second)
	f.poll() // second failure: backoff doubles to 2m
	if f.sum.count() != 2 {
		t.Fatalf("did not retry after the backoff: %d calls", f.sum.count())
	}
	f.clock.Advance(90 * time.Second)
	f.poll()
	if f.sum.count() != 2 {
		t.Errorf("the second backoff did not double: %d calls", f.sum.count())
	}
	f.clock.Advance(31 * time.Second)
	f.poll()
	if f.sum.count() != 3 {
		t.Errorf("calls = %d", f.sum.count())
	}
	// the delay is capped
	for range 6 {
		f.clock.Advance(9 * time.Minute)
		f.poll()
	}
	if f.sum.count() != 9 {
		t.Errorf("calls = %d; with the delay capped at 8 minutes, a 9-minute step always retries", f.sum.count())
	}
}

func TestNewContentRetriesAtOnceEvenWhileBackingOff(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.sum.set("", errors.New("down"))
	f.poll()
	changed := input()
	changed.Plan = "new plan"
	f.inputs.set("demo", changed)
	f.sum.set("- fine", nil)
	f.poll() // no clock advance: different content is not what failed
	if f.svc.Info("demo").Text != "- fine" {
		t.Errorf("info = %+v", f.svc.Info("demo"))
	}
}

func TestNoneNeverGathersAnythingOrPublishes(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	none := f.build(summary.None{})
	none.Poll(context.Background())
	if f.inputs.gather["demo"] != 0 {
		t.Error("none read the ticket's content")
	}
	if none.Enabled() || none.Info("demo") != nil || len(f.events()) != 0 {
		t.Error("none produced something")
	}
	// Run returns at once for none
	done := make(chan struct{})
	go func() { none.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return for the none provider")
	}
}

func TestTicketsWithNothingToSummarizeOrBrokenInputsAreSkipped(t *testing.T) {
	f := newFixture(t, "empty", "broken", "ok")
	f.inputs.set("empty", summary.Input{Ticket: "empty"})
	f.inputs.errs = map[string]error{"broken": errors.New("cannot read the diff")}
	f.inputs.set("ok", input())
	os.MkdirAll(filepath.Join(f.dir, ".hidden"), 0o700)
	os.MkdirAll(filepath.Join(f.dir, "no-yaml"), 0o700) // not a ticket
	f.poll()
	if f.sum.count() != 1 || f.svc.Info("ok") == nil {
		t.Errorf("asked %d times", f.sum.count())
	}
	if f.svc.Info("empty") != nil || f.svc.Info("broken") != nil {
		t.Error("a ticket with nothing to summarize got a summary")
	}
	if f.inputs.gather[".hidden"] != 0 || f.inputs.gather["no-yaml"] != 0 {
		t.Error("a non-ticket directory was looked at")
	}
}

func TestOneTicketsFailureDoesNotStopTheOthers(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.inputs.set("a", input())
	b := input()
	b.Title = "other"
	f.inputs.set("b", b)
	calls := 0
	f.svc = f.build(summarizerFunc(func(in summary.Input) (string, error) {
		calls++
		if in.Title == "Add locale" {
			return "", errors.New("boom")
		}
		return "- ok", nil
	}))
	f.poll()
	if calls != 2 || f.svc.Info("a").Error != "boom" || f.svc.Info("b").Text != "- ok" {
		t.Errorf("calls %d; a=%+v b=%+v", calls, f.svc.Info("a"), f.svc.Info("b"))
	}
}

type summarizerFunc func(summary.Input) (string, error)

func (summarizerFunc) Name() string  { return "ollama" }
func (summarizerFunc) Model() string { return "m" }
func (summarizerFunc) Remote() bool  { return false }
func (f summarizerFunc) Summarize(_ context.Context, in summary.Input) (string, error) {
	return f(in)
}

func TestASlowBackendTimesOutAndShuttingDownIsNotAFailure(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.sum.delay = 5 * time.Second // the fixture's request timeout is 200ms
	start := time.Now()
	f.poll()
	if time.Since(start) > 3*time.Second {
		t.Fatal("the request timeout did not apply")
	}
	if info := f.svc.Info("demo"); info == nil || !strings.Contains(info.Error, "deadline") {
		t.Errorf("info = %+v", info)
	}

	g := newFixture(t, "demo")
	g.inputs.set("demo", input())
	g.sum.delay = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	g.svc.Poll(ctx)
	if g.svc.Info("demo") != nil {
		t.Errorf("shutting down recorded a failure: %+v", g.svc.Info("demo"))
	}
}

func TestRemoteBackendsAreMarkedRemote(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	remote := f.build(&scripted{name: "claude", text: "- x"})
	remote.Poll(context.Background())
	if info := remote.Info("demo"); info == nil || !info.Remote || info.Backend != "claude" {
		t.Errorf("info = %+v", info)
	}
}

func TestEmptyAnswersCountAsFailures(t *testing.T) {
	f := newFixture(t, "demo")
	f.inputs.set("demo", input())
	f.sum.set("   ", nil)
	f.poll()
	if info := f.svc.Info("demo"); info == nil || info.Text != "" || !strings.Contains(info.Error, "returned nothing") {
		t.Errorf("info = %+v", info)
	}
}

func TestOtherRuntimeFieldsInTheTicketsStoreRecordAreKept(t *testing.T) {
	f := newFixture(t, "demo")
	f.store.Put("demo", []byte(`{"something_else":{"keep":"me"}}`))
	f.inputs.set("demo", input())
	f.poll()
	raw, _ := f.store.Get("demo")
	if !strings.Contains(string(raw), `"something_else":{"keep":"me"}`) || !strings.Contains(string(raw), `"summary"`) {
		t.Errorf("record = %s", raw)
	}
	if f.svc.Info("demo") == nil {
		t.Error("no summary")
	}
	// garbage in the store is replaced, not fatal
	f.store.Put("demo", []byte(`"a string"`))
	f.inputs.set("demo", func() summary.Input { in := input(); in.Plan = "x"; return in }())
	f.poll()
	if f.svc.Info("demo") == nil {
		t.Error("a malformed record broke the job")
	}
}
