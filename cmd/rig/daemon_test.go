package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/secrets"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

// rigEnv points rig at a private home with a short path (unix socket limit).
func rigEnv(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "rig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("RIG_HOME", home)
	t.Setenv("RIG_CONFIG", "")
	t.Setenv("HOME", t.TempDir())
	return home
}

// inProcessDaemon is a spawner that runs `rig daemon run` in a goroutine instead of a
// detached process, and a way to wait for it to exit.
type inProcessDaemon struct {
	backend  terminal.Backend // nil means the configured one
	docker   docker.Docker    // nil means the docker command
	keychain secrets.SecurityRunner
	gh       git.GH
	mu       sync.Mutex
	starts   int
	done     []chan int
	cancel   context.CancelFunc
}

func (d *inProcessDaemon) spawn(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	done := make(chan int, 1)
	d.done = append(d.done, done)
	go func() {
		done <- run(ctx, []string{"daemon", "run"}, &bytes.Buffer{}, &bytes.Buffer{}, deps{backend: d.backend, docker: d.docker, keychain: d.keychain, gh: d.gh})
	}()
	return nil
}

func (d *inProcessDaemon) shutdown(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.done = nil // waited on once; later calls are no-ops
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, c := range done {
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not exit")
		}
	}
}

func rig(t *testing.T, d deps, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut, d)
	return out.String(), errOut.String(), code
}

func TestDaemonStatusWhenStopped(t *testing.T) {
	rigEnv(t)
	stdout, stderr, code := rig(t, deps{}, "daemon", "status")
	if code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
	if stdout != "" || !strings.Contains(stderr, "not running") {
		t.Errorf("stdout = %q, stderr = %q; want the explanation on stderr only", stdout, stderr)
	}
}

func TestDaemonStopWhenStoppedIsANoOp(t *testing.T) {
	rigEnv(t)
	stdout, _, code := rig(t, deps{}, "daemon", "stop")
	if code != 0 || !strings.Contains(stdout, "not running") {
		t.Errorf("code = %d, stdout = %q; stopping a stopped daemon should succeed and say so", code, stdout)
	}
}

func TestDaemonStartStatusStopLifecycle(t *testing.T) {
	home := rigEnv(t)
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	deps := deps{spawner: d.spawn}

	stdout, stderr, code := rig(t, deps, "daemon", "start")
	if code != 0 || !strings.Contains(stdout, "running") {
		t.Fatalf("start: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	stdout, _, code = rig(t, deps, "daemon", "status")
	if code != 0 || !strings.Contains(stdout, "running") || !strings.Contains(stdout, "pid") {
		t.Errorf("status: code %d, stdout %q", code, stdout)
	}

	stdout, _, code = rig(t, deps, "daemon", "status", "--json")
	var status api.StatusResult
	if err := json.Unmarshal([]byte(stdout), &status); code != 0 || err != nil || status.PID != os.Getpid() || status.Version != "dev" {
		t.Errorf("status --json: code %d, %s, %v", code, stdout, err)
	}

	if _, _, code := rig(t, deps, "daemon", "start"); code != 0 {
		t.Errorf("starting an already running daemon should succeed, got %d", code)
	}
	if d.starts != 1 {
		t.Errorf("daemon spawned %d times, want 1", d.starts)
	}

	stdout, _, code = rig(t, deps, "daemon", "stop")
	if code != 0 || !strings.Contains(stdout, "stopped") {
		t.Errorf("stop: code %d, stdout %q", code, stdout)
	}
	d.shutdown(t)
	if _, err := os.Stat(filepath.Join(home, "rigd.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket remains after stop: %v", err)
	}
	if _, _, code := rig(t, deps, "daemon", "status"); code != 4 {
		t.Errorf("status after stop: exit code %d, want 4", code)
	}

	logText, err := os.ReadFile(filepath.Join(home, "rigd.log"))
	if err != nil || !strings.Contains(string(logText), "listening") {
		t.Errorf("rigd.log = %q, %v; the daemon should log to it", logText, err)
	}
}

func TestASecondDaemonRunFails(t *testing.T) {
	rigEnv(t)
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	if _, _, code := rig(t, deps{spawner: d.spawn}, "daemon", "start"); code != 0 {
		t.Fatal("could not start the daemon")
	}
	_, stderr, code := rig(t, deps{}, "daemon", "run")
	if code != 1 || !strings.Contains(stderr, "already running") {
		t.Errorf("code %d, stderr %q; want exit 1 saying the daemon is already running", code, stderr)
	}
}

func TestStartReportsASpawnFailureAsUnavailable(t *testing.T) {
	rigEnv(t)
	failing := deps{spawner: func(context.Context) error { return errors.New("no such binary") }}
	_, stderr, code := rig(t, failing, "daemon", "start")
	if code != 4 || !strings.Contains(stderr, "no such binary") {
		t.Errorf("code %d, stderr %q; want exit 4 with the cause", code, stderr)
	}
}
