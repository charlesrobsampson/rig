package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/testkit"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWaitReturnsWhenSomethingHappensOrTimesOut(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")

	if stdout, _, code := rig(t, d, "wait", "demo", "--phase", "plan"); code != 0 || !strings.Contains(stdout, "phase is plan") {
		t.Errorf("already in plan: code %d, %q", code, stdout)
	}
	if _, stderr, code := rig(t, d, "wait", "demo", "--phase-request", "--timeout", "300ms"); code != 1 || !strings.Contains(stderr, "timed out") {
		t.Errorf("timeout: code %d, %q", code, stderr)
	}

	var wg sync.WaitGroup
	var stdout string
	var code int
	wg.Add(1)
	go func() {
		defer wg.Done()
		stdout, _, code = rig(t, d, "wait", "demo", "--record", "note", "--phase-request", "--timeout", "10s", "--json")
	}()
	time.Sleep(700 * time.Millisecond)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"note","text":"a question for you"}`)
	wg.Wait()
	var got struct {
		Matched string `json:"matched"`
		Record  *struct {
			Text string `json:"text"`
		} `json:"record"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || got.Matched != "record" || got.Record == nil || got.Record.Text != "a question for you" {
		t.Errorf("code %d, %q, %v", code, stdout, err)
	}
}

func TestWaitNeedsSomethingToWaitFor(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	if _, stderr, code := rig(t, d, "wait", "demo"); code != 2 || !strings.Contains(stderr, "--record") {
		t.Errorf("code %d, %q", code, stderr)
	}
	if _, _, code := rig(t, d, "wait", "ghost", "--phase", "plan"); code != 5 {
		t.Errorf("unknown ticket: code %d", code)
	}
}

func TestWaitTakesSeveralPhasesOrReviewStatesAndEndsOnAnyOfThem(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	for _, args := range [][]string{
		{"--phase", "plan", "--phase", "implement"},
		{"--phase", "implement", "--phase", "plan"},
		{"--phase", "implement,plan"},
	} {
		stdout, stderr, code := rig(t, d, append([]string{"wait", "demo", "--timeout", "2s"}, args...)...)
		if code != 0 || !strings.Contains(stdout, "phase is plan") {
			t.Errorf("%v: code %d, %q %q", args, code, stdout, stderr)
		}
	}
	for _, args := range [][]string{{"--review", "passed", "--review", "done"}, {"--phase", "plan,nope"}} {
		if _, _, code := rig(t, d, append([]string{"wait", "demo", "--timeout", "1s"}, args...)...); code != 2 {
			t.Errorf("%v: code %d, want a usage error for the unknown value", args, code)
		}
	}
}

func TestWaitNewIgnoresARequestMadeBeforeTheWait(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"transition.request","data":{"to":"implement","reason":"plan ready"}}`)
	if _, _, code := rig(t, d, "wait", "demo", "--phase-request", "--timeout", "2s"); code != 0 {
		t.Errorf("an open request: exit %d", code)
	}
	if _, _, code := rig(t, d, "wait", "demo", "--phase-request", "--new", "--timeout", "1s"); code != 1 {
		t.Errorf("--new with only an older request: exit %d, want the timeout", code)
	}
}

// afterClock is a fake clock that never advances and remembers every After it was asked for.
type afterClock struct {
	*testkit.FakeClock
	mu     sync.Mutex
	afters []time.Duration
}

func (c *afterClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.afters = append(c.afters, d)
	c.mu.Unlock()
	return c.FakeClock.After(d)
}

func (c *afterClock) asked() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.afters...)
}

func TestWaitEndsOnAnEventWithoutAPollInterval(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	clk := &afterClock{FakeClock: testkit.NewFakeClock(time.Now())}
	d.clock = clk

	var wg sync.WaitGroup
	var stdout string
	var code int
	wg.Add(1)
	go func() {
		defer wg.Done()
		stdout, _, code = rig(t, d, "wait", "demo", "--record", "note", "--timeout", "1h", "--json")
	}()
	// The fake clock does not move; the wait must end because the record's timeline
	// entry reached it.
	waitFor(t, "the wait to hold its deadline", func() bool { return len(clk.asked()) > 0 })
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"note","text":"hello"}`)
	wg.Wait()
	if code != 0 || !strings.Contains(stdout, `"hello"`) {
		t.Fatalf("code %d, %q", code, stdout)
	}
	for _, a := range clk.asked() {
		if a != time.Hour {
			t.Errorf("wait asked the clock for %s; only the timeout is a timer", a)
		}
	}
}

func TestWaitAnswersAtOnceFromStateAndStillTimesOutOnTheClock(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	clk := &afterClock{FakeClock: testkit.NewFakeClock(time.Now())}
	d.clock = clk
	if stdout, _, code := rig(t, d, "wait", "demo", "--phase", "plan"); code != 0 || !strings.Contains(stdout, "phase is plan") {
		t.Errorf("code %d, %q", code, stdout)
	}
	done := make(chan int, 1)
	go func() {
		_, _, code := rig(t, d, "wait", "demo", "--phase-request", "--timeout", "10m")
		done <- code
	}()
	waitFor(t, "the second wait's deadline", func() bool { return len(clk.asked()) > 1 })
	clk.Advance(10 * time.Minute)
	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("code %d, want 1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not time out when the clock reached the deadline")
	}
}

// A timeline entry can reach the wait before the state change it records (the real
// daemon notes review.round.closed, then sets the status). The wait must not end on the
// entry while the state is old, and must end on the event published after the state
// changed, even though no further timeline entry follows.
func TestWaitEndsOnTheEventAfterTheStateChangedNotOnTheEntryBeforeIt(t *testing.T) {
	home := rigEnv(t)
	bus := events.NewBus(testkit.NewFakeClock(time.Now()), 0)
	srv, err := daemon.Listen(daemon.Options{SocketPath: filepath.Join(home, "rigd.sock"), PIDPath: filepath.Join(home, "rigd.pid"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	phase := domain.PhasePlan
	srv.Handle(api.MethodTicketGet, func(context.Context, json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return api.TicketGetResult{Summary: api.TicketSummary{ID: "demo", Phase: phase}}, nil
	})
	daemon.RegisterEventHandlers(srv, bus)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	clk := &afterClock{FakeClock: testkit.NewFakeClock(time.Now())}
	out := make(chan int, 1)
	var stdout lockedBuffer
	go func() {
		out <- run(context.Background(), []string{"wait", "demo", "--phase", "implement", "--timeout", "1h"}, &stdout, &lockedBuffer{}, deps{clock: clk})
	}()
	waitFor(t, "the subscription", func() bool { return bus.Subscribers() == 1 })
	waitFor(t, "the wait to hold its deadline", func() bool { return len(clk.asked()) > 0 })

	// The entry first, the state still old: no match.
	bus.Publish(api.EventTimelineEntry, "demo", "", api.TimelineEntryData{Ticket: "demo", Seq: 1})
	select {
	case code := <-out:
		t.Fatalf("the wait ended (code %d) on an entry while the state was old", code)
	case <-time.After(300 * time.Millisecond):
	}
	// Then the state changes and only a state event follows.
	mu.Lock()
	phase = domain.PhaseImplement
	mu.Unlock()
	bus.Publish(api.EventPhaseChanged, "demo", "", api.PhaseChangedData{From: domain.PhasePlan, To: domain.PhaseImplement})
	select {
	case code := <-out:
		if code != 0 || !strings.Contains(stdout.String(), "phase is implement") {
			t.Fatalf("code %d, %q", code, stdout.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end on the event published after the state changed")
	}
}

func TestWaitReturnsTheConnectErrorWhenTheStreamCannotBeReestablished(t *testing.T) {
	home := rigEnv(t)
	bus := events.NewBus(testkit.NewFakeClock(time.Now()), 0)
	srv, err := daemon.Listen(daemon.Options{SocketPath: filepath.Join(home, "rigd.sock"), PIDPath: filepath.Join(home, "rigd.pid"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv.Handle(api.MethodTicketGet, func(context.Context, json.RawMessage) (any, error) {
		return api.TicketGetResult{Summary: api.TicketSummary{ID: "demo", Phase: domain.PhasePlan}}, nil
	})
	daemon.RegisterEventHandlers(srv, bus)
	served := make(chan struct{})
	go func() { srv.Serve(context.Background()); close(served) }()

	clk := &afterClock{FakeClock: testkit.NewFakeClock(time.Now())}
	failing := func(context.Context) error { return errors.New("no such binary") }
	out := make(chan int, 1)
	var stderr lockedBuffer
	go func() {
		out <- run(context.Background(), []string{"wait", "demo", "--phase", "implement", "--timeout", "1h"}, &lockedBuffer{}, &stderr, deps{clock: clk, spawner: failing})
	}()
	waitFor(t, "the subscription", func() bool { return bus.Subscribers() == 1 })
	waitFor(t, "the wait to hold its deadline", func() bool { return len(clk.asked()) > 0 })
	srv.Stop() // the daemon goes away and stays away
	<-served
	waitFor(t, "the wait to rest before resubscribing", func() bool { return len(clk.asked()) > 1 })
	clk.Advance(waitRetry)
	select {
	case code := <-out:
		if code == 0 || !strings.Contains(stderr.String(), "no such binary") {
			t.Fatalf("code %d, stderr %q; want the connect error", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end when it could not reconnect")
	}
}

func TestWaitRecordTakesSeveralKindsAndQuestionEndsOnAnOpenQuestion(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")

	// --question with nothing asked times out
	if _, _, code := rig(t, d, "wait", "demo", "--question", "--timeout", "300ms"); code != 1 {
		t.Errorf("no question: exit %d, want the timeout", code)
	}

	// --record note,question ends on either kind
	for _, kind := range []string{"question", "note"} {
		var wg sync.WaitGroup
		var stdout string
		var code int
		wg.Add(1)
		go func() {
			defer wg.Done()
			stdout, _, code = rig(t, d, "wait", "demo", "--record", "note,question", "--timeout", "10s", "--json")
		}()
		time.Sleep(700 * time.Millisecond)
		rig(t, d, "agent-event", `{"kind":"`+kind+`","text":"see `+kind+`"}`)
		wg.Wait()
		if code != 0 || !strings.Contains(stdout, `"matched": "record"`) && !strings.Contains(stdout, `"matched":"record"`) || !strings.Contains(stdout, "see "+kind) {
			t.Errorf("%s: code %d, %q", kind, code, stdout)
		}
	}

	// the question left above is open: --question ends at once, naming it; --new does not
	stdout, _, code := rig(t, d, "wait", "demo", "--question", "--timeout", "2s")
	if code != 0 || !strings.Contains(stdout, "svc1 asks: see question") {
		t.Errorf("open question: code %d, %q", code, stdout)
	}
	if _, _, code := rig(t, d, "wait", "demo", "--question", "--new", "--timeout", "1s"); code != 1 {
		t.Errorf("--new with an older question: exit %d, want the timeout", code)
	}

	// answered: it no longer counts
	rig(t, d, "agent-event", "ready")
	if _, stderr, code := rig(t, d, "msg", "demo", "svc1", "do it"); code != 0 {
		t.Fatalf("msg: %d %q", code, stderr)
	}
	if _, _, code := rig(t, d, "wait", "demo", "--question", "--timeout", "500ms"); code != 1 {
		t.Errorf("answered question: exit %d, want the timeout", code)
	}

	// --new ends on a question asked after the wait began
	var wg sync.WaitGroup
	var out string
	var c int
	wg.Add(1)
	go func() {
		defer wg.Done()
		out, _, c = rig(t, d, "wait", "demo", "--question", "--new", "--timeout", "10s")
	}()
	time.Sleep(700 * time.Millisecond)
	rig(t, d, "agent-event", `{"kind":"question","text":"later one"}`)
	wg.Wait()
	if c != 0 || !strings.Contains(out, "later one") {
		t.Errorf("new question: code %d, %q", c, out)
	}
}

func TestACommandWarnsOnStderrWhenTheDaemonIsAnotherBuild(t *testing.T) {
	_, d, repo := newEnv(t)
	old := version
	version = "1.0.0" // the in-process daemon's build
	t.Cleanup(func() { version = old })
	newTicket(t, d, "demo", repo) // starts the daemon at 1.0.0

	d.version = "2.0.0"
	stdout, stderr, code := rig(t, d, "list", "--json")
	if code != 0 || strings.Count(stderr, "version 1.0.0 but this rig is 2.0.0") != 1 || !strings.Contains(stderr, "rig daemon restart") {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
	if strings.Contains(stderr, "-V") {
		t.Errorf("no vessel chosen, but the command names one: %q", stderr)
	}
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil || strings.Contains(stdout, "daemon is version") {
		t.Errorf("stdout is not clean JSON: %q (%v)", stdout, err)
	}

	// one line per process even though wait opens several connections
	_, stderr, _ = rig(t, d, "wait", "demo", "--phase", "plan", "--timeout", "2s")
	if n := strings.Count(stderr, "rig daemon restart"); n != 1 {
		t.Errorf("wait warned %d times: %q", n, stderr)
	}

	for _, ver := range []string{"1.0.0", "dev"} {
		d.version = ver
		if _, stderr, _ := rig(t, d, "list"); strings.Contains(stderr, "daemon") {
			t.Errorf("client %s: unexpected warning %q", ver, stderr)
		}
	}
}

func TestTheVersionWarningNamesTheVesselWhoseDaemonIsStale(t *testing.T) {
	work, _ := twoVessels(t)
	old := version
	version = "1.0.0"
	t.Cleanup(func() { version = old })
	startDaemon(t, work)
	d := deps{version: "2.0.0"}

	_, stderr, code := rig(t, d, "-V", "work", "daemon", "status")
	if code != 0 || !strings.Contains(stderr, `run "rig -V work daemon restart"`) {
		t.Errorf("-V work: code %d, stderr %q", code, stderr)
	}
	t.Setenv("RIG_VESSEL", "work")
	_, stderr, code = rig(t, d, "daemon", "status")
	if code != 0 || !strings.Contains(stderr, `run "rig -V work daemon restart"`) {
		t.Errorf("RIG_VESSEL=work: code %d, stderr %q", code, stderr)
	}
}

func TestWaitReviewNewEndsOnlyOnAVerdictReachedAfterTheWaitBegan(t *testing.T) {
	for _, tc := range []struct {
		status domain.ReviewStatus
		round  int // the round the new verdict is on; the standing one is round 2
	}{{domain.ReviewPassed, 3}, {domain.ReviewStopped, 3}, {domain.ReviewPassed, 2}, {domain.ReviewStopped, 2}} {
		status := tc.status
		t.Run(fmt.Sprintf("%s in round %d", status, tc.round), func(t *testing.T) {
			home := rigEnv(t)
			clk := &afterClock{FakeClock: testkit.NewFakeClock(time.Now())}
			bus := events.NewBus(clk, 0)
			srv, err := daemon.Listen(daemon.Options{SocketPath: filepath.Join(home, "rigd.sock"), PIDPath: filepath.Join(home, "rigd.pid"), Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			review := api.ReviewSummary{Status: status, Round: 2, At: clk.Now().Add(-time.Hour)}
			srv.Handle(api.MethodTicketGet, func(context.Context, json.RawMessage) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				r := review
				return api.TicketGetResult{Summary: api.TicketSummary{ID: "demo", Phase: domain.PhaseImplement, Review: &r}}, nil
			})
			daemon.RegisterEventHandlers(srv, bus)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			change := func(r api.ReviewSummary) {
				mu.Lock()
				review = r
				mu.Unlock()
				bus.Publish(api.EventReviewUpdated, "demo", "", api.ReviewUpdatedData{})
			}

			// Without --new the standing verdict ends the wait at once.
			var plain lockedBuffer
			if code := run(context.Background(), []string{"wait", "demo", "--review", string(status), "--timeout", "1h"}, &plain, &lockedBuffer{}, deps{clock: clk}); code != 0 {
				t.Fatalf("without --new: code %d, %q", code, plain.String())
			}

			out := make(chan int, 1)
			var stdout lockedBuffer
			go func() {
				out <- run(context.Background(), []string{"wait", "demo", "--review", string(status), "--new", "--timeout", "1h"}, &stdout, &lockedBuffer{}, deps{clock: clk})
			}()
			waitFor(t, "the subscription", func() bool { return bus.Subscribers() == 1 })
			waitFor(t, "the wait to hold its deadline", func() bool { return len(clk.asked()) > 1 })
			select {
			case code := <-out:
				t.Fatalf("--new ended (code %d) on the standing %s", code, status)
			case <-time.After(300 * time.Millisecond):
			}
			// The next round starts: not a verdict of the wanted kind.
			change(api.ReviewSummary{Status: domain.ReviewReviewing, Round: tc.round, At: clk.Now().Add(time.Minute)})
			select {
			case code := <-out:
				t.Fatalf("--new ended (code %d) on a round starting", code)
			case <-time.After(300 * time.Millisecond):
			}
			// Its verdict, on a later round or a later decision on the same one, ends it.
			change(api.ReviewSummary{Status: status, Round: tc.round, At: clk.Now().Add(2 * time.Minute)})
			select {
			case code := <-out:
				if code != 0 || !strings.Contains(stdout.String(), "review "+string(status)+", round "+strconv.Itoa(tc.round)) {
					t.Fatalf("code %d, %q", code, stdout.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("--new did not end on the next verdict")
			}
		})
	}
}
