package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// newEnv is a rig home, an in-process daemon that starts on demand, and a git repo.
func newEnv(t *testing.T) (home string, d deps, repo *testkit.Repo) {
	t.Helper()
	home, d, repo, _ = newEnvWithTerminal(t)
	return home, d, repo
}

// newEnvWithTerminal is newEnv with the in-memory terminal the daemon runs agents in
// and the exec seam recording what `rig attach` would run.
func newEnvWithTerminal(t *testing.T) (home string, d deps, repo *testkit.Repo, term *testkit.FakeTerminal) {
	t.Helper()
	home, d, repo, term, _ = newEnvFull(t)
	return home, d, repo, term
}

// newEnvFull also returns the in-memory Docker the daemon uses.
func newEnvFull(t *testing.T) (home string, d deps, repo *testkit.Repo, term *testkit.FakeTerminal, dk *testkit.FakeDocker) {
	t.Helper()
	home = rigEnv(t)
	term, dk = testkit.NewFakeTerminal(), testkit.NewFakeDocker()
	// an empty keychain: tests never read the real one
	empty := func(context.Context, ...string) (string, string, int, error) {
		return "", "item could not be found", 44, nil
	}
	gh := testkit.NewFakeGH()
	daemon := &inProcessDaemon{backend: term, docker: dk, keychain: empty, gh: gh}
	t.Cleanup(func() { daemon.shutdown(t) })
	return home, deps{spawner: daemon.spawn, backend: term, docker: dk, keychain: empty, gh: gh}, testkit.NewRepo(t), term, dk
}

func TestNewCreatesATicketThatListShows(t *testing.T) {
	home, d, repo := newEnv(t)
	stdout, stderr, code := rig(t, d, "new", "--id", "demo", "--title", "Demo ticket", "--service", "api="+repo.Dir)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	ticketDir := filepath.Join(home, "tickets", "demo")
	if !strings.Contains(stdout, "demo") || !strings.Contains(stdout, ticketDir) {
		t.Errorf("stdout = %q, want it to name the ticket and where it lives", stdout)
	}
	for _, f := range []string{"ticket.yaml", "state.json", "plan.md", "tasks.md", "manual-tests.md"} {
		if _, err := os.Stat(filepath.Join(ticketDir, f)); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
	}

	stdout, _, code = rig(t, d, "list")
	if code != 0 || !strings.Contains(stdout, "demo") || !strings.Contains(stdout, "plan") || !strings.Contains(stdout, "Demo ticket") {
		t.Errorf("list after new: code %d, stdout %q", code, stdout)
	}
}

func TestNewAcceptsATitleAsAPositionalArgument(t *testing.T) {
	home, d, repo := newEnv(t)
	_, stderr, code := rig(t, d, "new", "Fix the login bug", "--service", "api="+repo.Dir)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "tickets", "fix-the-login-bug", "ticket.yaml")); err != nil {
		t.Errorf("the id should be derived from the title: %v", err)
	}
}

func TestNewServiceFlagForms(t *testing.T) {
	home, d, _ := newEnv(t)
	backend, frontend := testkit.NewRepo(t), testkit.NewRepo(t)
	_, stderr, code := rig(t, d, "new", "--id", "multi", "--title", "Multi",
		"--service", "backend="+backend.Dir+"@feat/custom-branch",
		"--service", "frontend="+frontend.Dir)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	yaml, _ := os.ReadFile(filepath.Join(home, "tickets", "multi", "ticket.yaml"))
	for _, want := range []string{"name: backend", "name: frontend", "branch: feat/custom-branch", "branch: rig/multi/frontend", backend.Dir, frontend.Dir} {
		if !strings.Contains(string(yaml), want) {
			t.Errorf("ticket.yaml lacks %q:\n%s", want, yaml)
		}
	}
}

func TestNewTurnsRelativeRepoPathsIntoAbsoluteOnes(t *testing.T) {
	home, d, repo := newEnv(t)
	cwd, _ := os.Getwd()
	rel, err := filepath.Rel(cwd, repo.Dir)
	if err != nil {
		t.Skip("no relative path to the repo from here")
	}
	if _, stderr, code := rig(t, d, "new", "--id", "rel", "--title", "Rel", "--service", "api="+rel); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	yaml, _ := os.ReadFile(filepath.Join(home, "tickets", "rel", "ticket.yaml"))
	if !strings.Contains(string(yaml), "repo: "+repo.Dir) {
		t.Errorf("the daemon has a different working directory, so the repo path must be absolute:\n%s", yaml)
	}
}

func TestNewJSONPrintsTheAPIResult(t *testing.T) {
	home, d, repo := newEnv(t)
	stdout, _, code := rig(t, d, "new", "--id", "demo", "--title", "Demo", "--service", "api="+repo.Dir, "--json")
	var got api.TicketNewResult
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || got.Summary.ID != "demo" || got.Path != filepath.Join(home, "tickets", "demo") {
		t.Errorf("code %d, %q, %v", code, stdout, err)
	}
}

func TestNewErrors(t *testing.T) {
	_, d, repo := newEnv(t)
	if _, _, code := rig(t, d, "new", "--id", "taken", "--title", "T", "--service", "api="+repo.Dir); code != 0 {
		t.Fatal("setup failed")
	}
	notRepo := t.TempDir()

	tests := []struct {
		name string
		args []string
		code int
		text string
	}{
		{"no services", []string{"new", "--id", "x", "--title", "T"}, 2, "service"},
		{"a service without a name", []string{"new", "--id", "x", "--title", "T", "--service", repo.Dir}, 2, "name=repo"},
		{"a service without a repo", []string{"new", "--id", "x", "--title", "T", "--service", "api="}, 2, "name=repo"},
		{"an empty branch after @", []string{"new", "--id", "x", "--title", "T", "--service", "api=" + repo.Dir + "@"}, 2, "branch"},
		{"neither id nor title", []string{"new", "--service", "api=" + repo.Dir}, 2, "id or a title"},
		{"too many arguments", []string{"new", "a", "b", "--service", "api=" + repo.Dir}, 2, "arg"},
		{"an existing ticket", []string{"new", "--id", "taken", "--title", "T", "--service", "api=" + repo.Dir}, 1, "already exists"},
		{"a directory that is not a git repository", []string{"new", "--id", "x", "--title", "T", "--service", "api=" + notRepo}, 2, "not a git repository"},
		{"an unknown ticket source", []string{"new", "--from", "linear:ENG-1", "--service", "api=" + repo.Dir}, 2, "known: jira, manual"},
		{"a malformed --from", []string{"new", "--from", "nonsense", "--service", "api=" + repo.Dir}, 2, "type:ref"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := rig(t, d, tt.args...)
			if code != tt.code || !strings.Contains(stderr, tt.text) || stdout != "" {
				t.Errorf("code %d (want %d), stdout %q, stderr %q (want it to mention %q)", code, tt.code, stdout, stderr, tt.text)
			}
		})
	}
}

func TestNewCreatesAContractForMultiServiceTicketsOnly(t *testing.T) {
	home, d, repo := newEnv(t)
	other := testkit.NewRepo(t)
	if _, stderr, code := rig(t, d, "new", "--id", "solo", "--title", "One", "--service", "api="+repo.Dir); code != 0 {
		t.Fatalf("solo: %q", stderr)
	}
	if _, stderr, code := rig(t, d, "new", "--id", "duo", "--title", "Two", "--service", "api="+repo.Dir, "--service", "web="+other.Dir); code != 0 {
		t.Fatalf("duo: %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "tickets", "solo", "contract.md")); !os.IsNotExist(err) {
		t.Errorf("a single-service ticket got a contract: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "tickets", "duo", "contract.md"))
	if err != nil || !strings.Contains(string(raw), "# Contract: duo") || !strings.Contains(string(raw), "## Proposed changes") {
		t.Errorf("duo's contract: %v\n%s", err, raw)
	}
}
