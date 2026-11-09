package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// startedService is a ticket "demo" whose service svc1 runs in the fake Docker.
func startedService(t *testing.T) (d deps, dk *testkit.FakeDocker) {
	t.Helper()
	home, d, repo, _, dk := newEnvFull(t)
	repo.Commit("Dockerfile", "FROM scratch\n", "add a Dockerfile")
	newTicket(t, d, "demo", repo)
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	os.WriteFile(yamlPath, []byte(strings.Replace(string(raw), "      branch: rig/", "      build: {dockerfile: Dockerfile}\n      branch: rig/", 1)), 0o600)
	if _, stderr, code := rig(t, d, "start", "demo"); code != 0 {
		t.Fatalf("start: code %d, %q", code, stderr)
	}
	return d, dk
}

func TestLogsPrefixesEachLineWithItsService(t *testing.T) {
	d, dk := startedService(t)
	dk.Print("rig-demo-svc1", "server listening\nGET / 200")
	stdout, stderr, code := rig(t, d, "logs", "demo")
	if code != 0 || stdout != "svc1 | server listening\nsvc1 | GET / 200\n" {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if stdout, _, code := rig(t, d, "logs", "demo", "svc1", "--tail", "1"); code != 0 || stdout != "svc1 | GET / 200\n" {
		t.Errorf("--tail 1: code %d, %q", code, stdout)
	}
	stdout, _, _ = rig(t, d, "logs", "demo", "--json")
	var first api.LogLine
	if err := json.Unmarshal([]byte(strings.SplitN(stdout, "\n", 2)[0]), &first); err != nil || first.Service != "svc1" || first.Line != "server listening" {
		t.Errorf("--json: %q, %v", stdout, err)
	}
}

func TestLogsErrorsExitWithTheRightCodes(t *testing.T) {
	d, _ := startedService(t)
	tests := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"an unknown ticket", []string{"logs", "ghost"}, 5, "ghost"},
		{"an unknown service", []string{"logs", "demo", "nope"}, 5, "svc1"},
		{"a negative tail", []string{"logs", "demo", "--tail", "-3"}, 2, "tail"},
		{"no arguments", []string{"logs"}, 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stderr, code := rig(t, d, tt.args...)
			if code != tt.code || !strings.Contains(stderr, tt.msg) {
				t.Errorf("code %d (want %d), stderr %q", code, tt.code, stderr)
			}
		})
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestLogsFollowStreamsUntilInterrupted(t *testing.T) {
	d, dk := startedService(t)
	dk.Print("rig-demo-svc1", "first")
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"logs", "demo", "-f"}, &out, &errOut, d) }()

	wait := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if strings.Contains(out.String(), want) {
				return
			}
		}
		t.Fatalf("never saw %q in %q", want, out.String())
	}
	wait("svc1 | first")
	dk.Print("rig-demo-svc1", "second, printed while following")
	wait("svc1 | second, printed while following")
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d after Ctrl-C, stderr %q", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs -f did not stop")
	}
}
