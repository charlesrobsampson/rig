package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
)

// stubVessel serves a daemon whose busy answer a test chooses (nil: it does not know
// daemon.busy, as an older build) and counts the daemons started by its spawner.
type stubVessel struct {
	t      *testing.T
	home   string
	mu     sync.Mutex
	starts int
	stops  int
	cancel []context.CancelFunc
	done   []chan error
}

func (v *stubVessel) serve(busy *api.DaemonBusyResult) {
	srv, err := daemon.Listen(daemon.Options{SocketPath: filepath.Join(v.home, "rigd.sock"), PIDPath: filepath.Join(v.home, "rigd.pid"), Version: "test"})
	if err != nil {
		v.t.Fatal(err)
	}
	if busy != nil {
		srv.Handle(api.MethodDaemonBusy, func(context.Context, json.RawMessage) (any, error) { return *busy, nil })
	}
	srv.Handle(api.MethodDaemonStop, func(context.Context, json.RawMessage) (any, error) {
		v.mu.Lock()
		v.stops++
		v.mu.Unlock()
		srv.Stop()
		return api.EmptyData{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	v.mu.Lock()
	v.cancel, v.done = append(v.cancel, cancel), append(v.done, done)
	v.mu.Unlock()
}

func newStubVessel(t *testing.T, busy *api.DaemonBusyResult) (*stubVessel, deps) {
	v := &stubVessel{t: t, home: rigEnv(t)}
	v.serve(busy)
	t.Cleanup(func() {
		for i, c := range v.cancel {
			c()
			<-v.done[i]
		}
	})
	return v, deps{spawner: func(context.Context) error {
		v.mu.Lock()
		v.starts++
		v.mu.Unlock()
		v.serve(&api.DaemonBusyResult{})
		return nil
	}}
}

func TestRestartRefusesWithWorkInFlightAndGoesAheadWithForce(t *testing.T) {
	busy := &api.DaemonBusyResult{Tickets: []api.InFlight{{Ticket: "t7", Doing: "a review round is starting"}, {Ticket: "t9", Doing: "an operation is under way (start, stop or sync)"}}}
	v, d := newStubVessel(t, busy)

	stdout, stderr, code := rig(t, d, "daemon", "restart")
	if code == 0 || stdout != "" || !strings.Contains(stderr, "t7, t9") || !strings.Contains(stderr, "--force") {
		t.Fatalf("restart: code %d, %q, %q; want a refusal naming both tickets", code, stdout, stderr)
	}
	if v.stops != 0 || v.starts != 0 {
		t.Fatalf("a refused restart stopped %d and started %d daemons", v.stops, v.starts)
	}
	if _, _, code := rig(t, d, "daemon", "status"); code != 0 {
		t.Fatalf("the daemon is not up after a refusal: %d", code)
	}

	stdout, stderr, code = rig(t, d, "daemon", "restart", "--force")
	if code != 0 || !strings.Contains(stdout, "running") || v.stops != 1 || v.starts != 1 {
		t.Fatalf("restart --force: code %d, %q, %q, stops %d starts %d", code, stdout, stderr, v.stops, v.starts)
	}
}

func TestRestartOfAnOlderDaemonCannotBeCheckedSoNeedsForce(t *testing.T) {
	v, d := newStubVessel(t, nil)
	_, stderr, code := rig(t, d, "daemon", "restart")
	if code == 0 || !strings.Contains(stderr, "too old") || !strings.Contains(stderr, "--force") || v.stops != 0 {
		t.Fatalf("restart: code %d, %q, stops %d", code, stderr, v.stops)
	}
	if _, stderr, code := rig(t, d, "daemon", "restart", "--force"); code != 0 || v.stops != 1 || v.starts != 1 {
		t.Fatalf("restart --force: code %d, %q, stops %d starts %d", code, stderr, v.stops, v.starts)
	}
}

func TestRestartOnAnIdleVesselBringsTheNewVersionUp(t *testing.T) {
	rigEnv(t)
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	old := version
	t.Cleanup(func() { version = old })

	version = "1.0.0"
	if _, stderr, code := rig(t, deps{spawner: d.spawn}, "daemon", "start"); code != 0 {
		t.Fatalf("start: %d %q", code, stderr)
	}
	version = "2.0.0"
	stdout, stderr, code := rig(t, deps{spawner: d.spawn}, "daemon", "restart")
	if code != 0 || !strings.Contains(stdout, "rigd 2.0.0 running") || d.starts != 2 {
		t.Fatalf("restart: code %d, %q, %q, starts %d", code, stdout, stderr, d.starts)
	}
	stdout, _, code = rig(t, deps{spawner: d.spawn}, "daemon", "status", "--json")
	var st api.StatusResult
	if json.Unmarshal([]byte(stdout), &st) != nil || code != 0 || st.Version != "2.0.0" {
		t.Fatalf("status after: %d %q", code, stdout)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("RIG_HOME"), "rigd.sock")); err != nil {
		t.Fatalf("no socket after the restart: %v", err)
	}
}

func TestRestartOfADaemonThatIsNotRunningStartsIt(t *testing.T) {
	rigEnv(t)
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	stdout, stderr, code := rig(t, deps{spawner: d.spawn}, "daemon", "restart")
	if code != 0 || !strings.Contains(stdout, "running") || d.starts != 1 {
		t.Fatalf("code %d, %q, %q, starts %d", code, stdout, stderr, d.starts)
	}
}
