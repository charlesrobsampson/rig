package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func startedTicket(t *testing.T) (home string, d deps, repo *testkit.Repo, checkout string) {
	t.Helper()
	home, d, repo = newEnv(t)
	newTicket(t, d, "demo", repo)
	if _, stderr, code := rig(t, d, "start", "demo"); code != 0 {
		t.Fatalf("start failed: %s", stderr)
	}
	return home, d, repo, filepath.Join(home, "tickets", "demo", "svc1")
}

func TestStopImportsWorkAndRemovesTheCheckout(t *testing.T) {
	_, d, repo, checkout := startedTicket(t)
	os.WriteFile(filepath.Join(checkout, "work.txt"), []byte("agent work\n"), 0o644)
	testkit.RunGit(t, checkout, "add", "work.txt")
	testkit.RunGit(t, checkout, "commit", "-q", "-m", "agent commit")
	sha := testkit.RunGit(t, checkout, "rev-parse", "HEAD")

	stdout, stderr, code := rig(t, d, "stop", "demo")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "svc1") || !strings.Contains(stdout, "yes") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Error("the checkout remains")
	}
	if tip := repo.Git("rev-parse", "rig/demo/svc1"); tip != sha {
		t.Errorf("the main repo's branch is at %q, want the agent's commit %q", tip, sha)
	}
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "svc1:stopped") {
		t.Errorf("list after stop:\n%s", listing)
	}
}

func TestStopRefusesUncommittedWorkAndExplainsPerService(t *testing.T) {
	_, d, _, checkout := startedTicket(t)
	os.WriteFile(filepath.Join(checkout, "README.md"), []byte("edited\n"), 0o644)

	stdout, stderr, code := rig(t, d, "stop", "demo")
	if code != 3 || stdout != "" {
		t.Fatalf("code %d (want 3), stdout %q", code, stdout)
	}
	for _, want := range []string{"still holds work", "  - svc1:", "1 modified file(s)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(checkout); err != nil {
		t.Error("a refused stop removed the checkout")
	}
}

func TestStopForceAsksFirstUnlessYes(t *testing.T) {
	_, d, _, checkout := startedTicket(t)
	os.WriteFile(filepath.Join(checkout, "scratch.txt"), []byte("lost"), 0o644)

	t.Run("no answer means no", func(t *testing.T) {
		d := d
		d.stdin = strings.NewReader("")
		_, stderr, code := rig(t, d, "stop", "demo", "--force")
		if code != 1 || !strings.Contains(stderr, "Continue?") || !strings.Contains(stderr, "aborted") {
			t.Errorf("code %d, stderr %q", code, stderr)
		}
		if _, err := os.Stat(checkout); err != nil {
			t.Error("the checkout was removed without confirmation")
		}
	})

	t.Run("an explicit no", func(t *testing.T) {
		d := d
		d.stdin = strings.NewReader("n\n")
		if _, _, code := rig(t, d, "stop", "demo", "--force"); code != 1 {
			t.Errorf("code %d, want 1", code)
		}
	})

	t.Run("yes at the prompt", func(t *testing.T) {
		d := d
		d.stdin = strings.NewReader("y\n")
		if _, stderr, code := rig(t, d, "stop", "demo", "--force"); code != 0 {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		if _, err := os.Stat(checkout); !os.IsNotExist(err) {
			t.Error("the confirmed stop left the checkout")
		}
	})
}

func TestStopForceYesSkipsThePrompt(t *testing.T) {
	_, d, _, checkout := startedTicket(t)
	os.WriteFile(filepath.Join(checkout, "scratch.txt"), []byte("lost"), 0o644)
	d.stdin = strings.NewReader("") // nobody to ask
	if _, stderr, code := rig(t, d, "stop", "demo", "--force", "--yes"); code != 0 || strings.Contains(stderr, "Continue?") {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
}

func TestStopKeepCheckouts(t *testing.T) {
	_, d, _, checkout := startedTicket(t)
	stdout, _, code := rig(t, d, "stop", "demo", "--keep-checkouts")
	if code != 0 || !strings.Contains(stdout, "no") {
		t.Errorf("code %d, stdout %q", code, stdout)
	}
	if _, err := os.Stat(checkout); err != nil {
		t.Error("--keep-checkouts removed the directory")
	}
}

func TestStopJSON(t *testing.T) {
	_, d, _, _ := startedTicket(t)
	stdout, _, code := rig(t, d, "stop", "demo", "--json")
	var got api.TicketStopResult
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || len(got.Services) != 1 || !got.Services[0].Removed {
		t.Errorf("code %d, %q, %v", code, stdout, err)
	}
}

func TestStopErrors(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "fresh", repo)
	tests := []struct {
		name string
		args []string
		code int
		text string
	}{
		{"a ticket that was never started", []string{"stop", "fresh"}, 1, "nothing to stop"},
		{"an unknown ticket", []string{"stop", "nope"}, 5, "nope"},
		{"no ticket id", []string{"stop"}, 2, "arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := rig(t, d, tt.args...)
			if code != tt.code || !strings.Contains(stderr, tt.text) || stdout != "" {
				t.Errorf("code %d (want %d), stdout %q, stderr %q", code, tt.code, stdout, stderr)
			}
		})
	}
}

func TestStopListsWhatItRemovedAndStoppingTwiceIsQuiet(t *testing.T) {
	_, d, repo, _, _ := newEnvFull(t)
	repo.Commit("Dockerfile", "FROM scratch\n", "add a Dockerfile")
	newTicket(t, d, "demo", repo)
	if _, stderr, code := rig(t, d, "start", "demo"); code != 0 {
		t.Fatalf("start: %q", stderr)
	}
	stdout, stderr, code := rig(t, d, "stop", "demo")
	if code != 0 || !strings.Contains(stdout, "removed: container rig-demo-svc1-agent, network rig-demo") {
		t.Errorf("first stop: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = rig(t, d, "stop", "demo")
	if code != 0 || !strings.Contains(stdout, "demo is already stopped") || strings.Contains(stdout, "removed:") {
		t.Errorf("second stop: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, _, code := rig(t, d, "stop", "ghost"); code != 5 {
		t.Errorf("an unknown ticket: code %d", code)
	}
}
