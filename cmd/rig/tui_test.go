package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/tui"
)

func TestBareRigOnAPipePrintsTheListInsteadOfTheTUI(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	opened := false
	d.isTerminal = func() bool { return false }
	d.runTUI = func(context.Context, tui.Options) error { opened = true; return nil }
	stdout, stderr, code := rig(t, d)
	if code != 0 || opened || !strings.Contains(stdout, "demo") || !strings.Contains(stdout, "PHASE") {
		t.Errorf("code %d, opened %v, stdout %q, stderr %q", code, opened, stdout, stderr)
	}
}

func TestBareRigWithJSONNeverOpensTheTUI(t *testing.T) {
	_, d, _ := newEnv(t)
	opened := false
	d.isTerminal = func() bool { return true }
	d.runTUI = func(context.Context, tui.Options) error { opened = true; return nil }
	stdout, _, code := rig(t, d, "--json")
	if code != 0 || opened || !strings.Contains(stdout, `"tickets"`) {
		t.Errorf("code %d, opened %v, stdout %q", code, opened, stdout)
	}
}

func TestBareRigOnATerminalOpensTheTUIOnTheDaemon(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	d.isTerminal = func() bool { return true }
	var got []string
	d.runTUI = func(ctx context.Context, o tui.Options) error {
		tickets, err := o.Source.Tickets(ctx)
		if err != nil {
			return err
		}
		for _, tk := range tickets {
			got = append(got, tk.ID)
		}
		return nil
	}
	if _, stderr, code := rig(t, d); code != 0 || strings.Join(got, ",") != "demo" {
		t.Errorf("code %d, tickets %v, stderr %q", code, got, stderr)
	}
}

func TestBareRigRejectsStrayArguments(t *testing.T) {
	_, d, _ := newEnv(t)
	if _, _, code := rig(t, d, "bogus"); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

// source is the TUI's data source as the in-process daemon serves it.
func source(d deps) tui.Source { return daemonSource{&app{deps: d}} }

func TestTheTUIReadsLifecycleFilesThroughTheDaemon(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	os.WriteFile(filepath.Join(home, "tickets", "demo", "plan.md"), []byte("# My plan\n"), 0o600)
	src := source(d)

	got, err := src.File(context.Background(), "demo", "plan.md")
	if err != nil || got != "# My plan\n" {
		t.Errorf("plan.md = %q, %v", got, err)
	}
	if _, err := src.File(context.Background(), "demo", "contract.md"); !errors.Is(err, tui.ErrMissing) {
		t.Errorf("a single-service ticket has no contract: %v", err)
	}
	if _, err := src.File(context.Background(), "ghost", "plan.md"); !errors.Is(err, tui.ErrMissing) {
		t.Errorf("an unknown ticket: %v", err)
	}
	if _, err := src.File(context.Background(), "demo", "state.json"); err == nil || errors.Is(err, tui.ErrMissing) {
		t.Errorf("state.json must be refused, not reported missing: %v", err)
	}
}

func TestTheTUIShowsTheSameDiffAsRigReview(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	d.delta = func(string, io.Writer) (bool, error) { return false, nil } // delta is not installed
	rig(t, d, "start", "demo", "--sandbox", "host")
	dir := filepath.Join(home, "tickets", "demo", "svc1")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("hello\n"), 0o600)
	testkit.RunGit(t, dir, "add", "-A")
	testkit.RunGit(t, dir, "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-qm", "work")

	got, err := source(d).Diff(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"== svc1", "feature.txt", "+hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}
	if _, err := source(d).Diff(context.Background(), "ghost"); !errors.Is(err, tui.ErrMissing) {
		t.Errorf("an unknown ticket's diff: %v", err)
	}
}

func TestTheTUIShowsContainerLogs(t *testing.T) {
	_, d, repo, _, _ := newEnvFull(t)
	newTicket(t, d, "demo", repo)
	if _, err := source(d).Logs(context.Background(), "ticket-that-does-not-exist"); !errors.Is(err, tui.ErrMissing) {
		t.Errorf("an unknown ticket's logs: %v", err)
	}
	// started without containers there is nothing to show
	rig(t, d, "start", "demo", "--sandbox", "host")
	if _, err := source(d).Logs(context.Background(), "demo"); !errors.Is(err, tui.ErrMissing) {
		t.Errorf("a ticket without containers: %v", err)
	}
}
