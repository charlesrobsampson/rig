package main

import (
	"encoding/json"
	"github.com/charlesrobsampson/rig/internal/testkit"
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
	start := time.Now()
	if _, stderr, code := rig(t, d, "wait", "demo", "--phase-request", "--timeout", "300ms"); code != 1 || !strings.Contains(stderr, "timed out") {
		t.Errorf("timeout: code %d, %q", code, stderr)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the timeout was not kept")
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
