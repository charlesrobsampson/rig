package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// eventsDaemon runs a daemon with just the events handler on the rig home's socket.
func eventsDaemon(t *testing.T, home string) *events.Bus {
	t.Helper()
	bus := events.NewBus(testkit.NewFakeClock(time.Date(2026, 11, 6, 12, 30, 15, 0, time.UTC)), 0)
	srv, err := daemon.Listen(daemon.Options{SocketPath: filepath.Join(home, "rigd.sock"), PIDPath: filepath.Join(home, "rigd.pid"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	daemon.RegisterEventHandlers(srv, bus)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return bus
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(testkit.WaitLimit)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// streamEvents runs `rig events` until stop is called and returns its exit code.
func streamEvents(t *testing.T, args ...string) (stdout, stderr *lockedBuffer, stop func() int) {
	t.Helper()
	stdout, stderr = &lockedBuffer{}, &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() { code <- run(ctx, append([]string{"events"}, args...), stdout, stderr, deps{}) }()
	return stdout, stderr, func() int {
		cancel()
		select {
		case c := <-code:
			return c
		case <-time.After(testkit.WaitLimit):
			t.Fatal("rig events did not exit")
			return -1
		}
	}
}

func TestEventsPrintsEventsAsTheyHappen(t *testing.T) {
	home := rigEnv(t)
	bus := eventsDaemon(t, home)
	stdout, stderr, stop := streamEvents(t)

	waitFor(t, "the subscription", func() bool { return bus.Subscribers() == 1 })
	bus.Publish(api.EventPhaseChanged, "ticket-412", "", api.PhaseChangedData{From: domain.PhasePlan, To: domain.PhaseImplement, By: "human"})
	bus.Publish(api.EventAgentState, "ticket-412", "backend", api.AgentStateData{State: domain.AgentBlocked, Previous: domain.AgentWorking})
	waitFor(t, "both events", func() bool { return strings.Contains(stdout.String(), "agent.state") })

	if code := stop(); code != 0 {
		t.Errorf("exit code = %d, want 0 after Ctrl-C", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout has %d lines, want one per event (the ack is not an event to show):\n%s", len(lines), stdout.String())
	}
	localTime := time.Date(2026, 11, 6, 12, 30, 15, 0, time.UTC).Local().Format("15:04:05")
	for _, want := range []string{localTime, "phase.changed", "ticket-412", "plan", "implement"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q lacks %q", lines[0], want)
		}
	}
	for _, want := range []string{"agent.state", "ticket-412/backend", "blocked"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("line %q lacks %q", lines[1], want)
		}
	}
	if !strings.Contains(stderr.String(), "listening") {
		t.Errorf("stderr = %q; a note that the stream is live belongs there, not on stdout", stderr.String())
	}
}

func TestEventsJSONPrintsOneEventPerLine(t *testing.T) {
	home := rigEnv(t)
	bus := eventsDaemon(t, home)
	stdout, _, stop := streamEvents(t, "--json")

	waitFor(t, "the subscription", func() bool { return bus.Subscribers() == 1 })
	bus.Publish(api.EventTicketStarted, "ticket-1", "", api.EmptyData{})
	waitFor(t, "the event", func() bool { return strings.Contains(stdout.String(), "ticket.started") })
	stop()

	var types []api.EventType
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var ev api.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %q is not a JSON event: %v", line, err)
		}
		types = append(types, ev.Type)
	}
	if len(types) != 2 || types[0] != api.EventSubscribed || types[1] != api.EventTicketStarted {
		t.Errorf("types = %v, want the ack then ticket.started", types)
	}
}

func TestEventsExitsWithAnErrorWhenTheDaemonGoesAway(t *testing.T) {
	home := rigEnv(t)
	srvDone := make(chan struct{})
	bus := events.NewBus(testkit.NewFakeClock(time.Now()), 0) // the fake clock starts at real now so it is not before the daemon records real times; no elapsed time is compared
	srv, err := daemon.Listen(daemon.Options{SocketPath: filepath.Join(home, "rigd.sock"), PIDPath: filepath.Join(home, "rigd.pid"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	daemon.RegisterEventHandlers(srv, bus)
	go func() { srv.Serve(context.Background()); close(srvDone) }()

	_, stderr, stop := streamEvents(t)
	waitFor(t, "the subscription", func() bool { return bus.Subscribers() == 1 })
	srv.Stop()
	<-srvDone
	// The client must notice on its own; cancelling it first would end it with 0.
	waitFor(t, "the client to see the daemon go", func() bool {
		msg := stderr.String()
		return strings.Contains(msg, "shutting down") || strings.Contains(msg, "connection lost")
	})

	code := stop()
	if msg := stderr.String(); code != 4 || !(strings.Contains(msg, "shutting down") || strings.Contains(msg, "connection lost")) {
		t.Errorf("exit code %d, stderr %q; want 4 and a message that the daemon went away", code, msg)
	}
}

// watchWith runs `rig watch` against the in-process daemon until stop is called.
func watchWith(t *testing.T, d deps, args ...string) (stdout *lockedBuffer, stop func() int) {
	t.Helper()
	stdout = &lockedBuffer{}
	stderr := &lockedBuffer{}
	defer waitFor(t, "the subscription", func() bool {
		return strings.Contains(stderr.String(), "listening") || strings.Contains(stdout.String(), "subscribed")
	})
	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() { code <- run(ctx, append([]string{"watch"}, args...), stdout, stderr, d) }()
	return stdout, func() int {
		cancel()
		select {
		case c := <-code:
			return c
		case <-time.After(testkit.WaitLimit):
			t.Fatal("rig watch did not exit")
			return -1
		}
	}
}

func TestWatchATicketReplaysFromSinceThenShowsEntriesLive(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	newTicket(t, d, "other", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	rig(t, d, "start", "other", "--sandbox", "host")
	var res api.TimelineListResult
	tl, _, _ := rig(t, d, "timeline", "demo", "--json")
	if err := json.Unmarshal([]byte(tl), &res); err != nil || len(res.Entries) == 0 {
		t.Fatalf("timeline %q: %v", tl, err)
	}

	stdout, stop := watchWith(t, d, "demo", "--since", "0", "--json")
	waitFor(t, "the replay", func() bool { return strings.Count(stdout.String(), `"timeline.entry"`) >= len(res.Entries) })
	t.Setenv("RIG_TICKET", "other")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"note","text":"not for demo"}`)
	t.Setenv("RIG_TICKET", "demo")
	rig(t, d, "agent-event", `{"kind":"note","text":"for demo"}`)
	waitFor(t, "the live entry", func() bool { return strings.Contains(stdout.String(), "for demo") })
	if code := stop(); code != 0 {
		t.Errorf("exit %d", code)
	}
	var seqs []int
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var ev api.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if ev.Type != api.EventTimelineEntry {
			continue
		}
		if ev.Ticket != "demo" {
			t.Errorf("an event of %s came through", ev.Ticket)
		}
		var td api.TimelineEntryData
		ev.DecodeData(&td)
		seqs = append(seqs, td.Seq)
	}
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("seqs %v: want 1, 2, 3, ... once each", seqs)
		}
	}
	if strings.Contains(stdout.String(), "not for demo") {
		t.Error("another ticket's entry was shown")
	}
}

func TestWatchWithoutSinceShowsOnlyNewEntries(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	stdout, stop := watchWith(t, d, "demo")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"note","text":"fresh"}`)
	waitFor(t, "the live entry", func() bool { return strings.Contains(stdout.String(), "fresh") })
	stop()
	if strings.Contains(stdout.String(), "ticket.started") {
		t.Errorf("the past was replayed without --since:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "journal.recorded") || !strings.Contains(stdout.String(), "demo") {
		t.Errorf("output: %s", stdout.String())
	}
}

func TestWatchSinceNeedsATicket(t *testing.T) {
	rigEnv(t)
	if _, _, code := rig(t, deps{}, "watch", "--since", "3"); code != 2 {
		t.Errorf("code %d, want 2", code)
	}
}
