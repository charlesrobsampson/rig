package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
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

func TestTheTUIActionsDoWhatTheCLIDoes(t *testing.T) {
	home, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	other := testkit.NewRepo(t)
	newTicket(t, d, "multi", repo, other)
	ctx := context.Background()
	src := source(d)

	// approve: the plan gate of a single-service ticket
	if err := src.Approve(ctx, "demo", domain.GatePlanApproval); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out, _, _ := rig(t, d, "gates", "demo"); !strings.Contains(out, "approved") {
		t.Errorf("gates after approve:\n%s", out)
	}
	// ... which is refused, with reasons, for a multi-service ticket with no contract
	err := src.Approve(ctx, "multi", domain.GatePlanApproval)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeBlocked {
		t.Errorf("approve with no contract = %v, want blocked", err)
	}

	// phase
	if err := src.MovePhase(ctx, "demo", domain.PhaseImplement); err != nil {
		t.Errorf("move: %v", err)
	}

	// message and attach need a running agent
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", "idle")
	if err := src.Message(ctx, "demo", "svc1", "hello agent"); err != nil {
		t.Errorf("message: %v", err)
	}
	panes, _ := term.List(ctx)
	if in := term.Input(panes[0].ID); len(in) == 0 || in[0].Text != "[rig] hello agent" {
		t.Errorf("typed %v", in)
	}
	if cmd, err := src.AttachCommand(ctx, "demo", "svc1"); err != nil || len(cmd) == 0 {
		t.Errorf("attach = %v, %v", cmd, err)
	}
	if _, err := src.AttachCommand(ctx, "demo", "nope"); err == nil {
		t.Error("attach to a service that does not exist succeeded")
	}

	// stop refuses unsaved work and --force discards it, as `rig stop` does
	dirty := filepath.Join(home, "tickets", "demo", "svc1", "wip.txt")
	os.WriteFile(dirty, []byte("x"), 0o600)
	_, err = src.Stop(ctx, "demo", false)
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeBlocked || len(apiErr.Details) == 0 {
		t.Errorf("stop with unsaved work = %v, want blocked with details", err)
	}
	if _, err := src.Stop(ctx, "demo", true); err != nil {
		t.Errorf("forced stop: %v", err)
	}
	if _, err := os.Stat(dirty); err == nil {
		t.Error("the forced stop left the checkout")
	}
}

func TestTheTUIResolvesContractProposalsThroughTheDaemon(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "multi", repo, testkit.NewRepo(t))
	md := "# Contract: multi\n\n## Agreed\nBase.\n\n## Proposed changes\n### P-svc1-1 · from svc1 · open\n**What:** add a field\n**Why:** y\n\n### P-svc2-1 · from svc2 · open\n**What:** drop it\n**Why:** z\n\n## Changelog\n"
	os.WriteFile(filepath.Join(home, "tickets", "multi", "contract.md"), []byte(md), 0o600)
	ctx := context.Background()
	src := source(d)

	got, err := src.Proposals(ctx, "multi")
	if err != nil || len(got) != 2 || got[0].ID != "P-svc1-1" || got[0].Status != "open" {
		t.Fatalf("proposals = %+v, %v", got, err)
	}
	if err := src.Resolve(ctx, "multi", "P-svc1-1", true, ""); err != nil {
		t.Errorf("accept: %v", err)
	}
	if err := src.Resolve(ctx, "multi", "P-svc2-1", false, ""); err == nil {
		t.Error("a rejection without a reason went through")
	}
	if err := src.Resolve(ctx, "multi", "P-svc2-1", false, "not now"); err != nil {
		t.Errorf("reject: %v", err)
	}
	got, _ = src.Proposals(ctx, "multi")
	if got[0].Status != "accepted" || got[1].Status != "rejected" || got[1].Rejected != "not now" {
		t.Errorf("after: %+v", got)
	}
	// a single-service ticket has no contract: the TUI's "missing"
	newTicket(t, d, "solo", repo)
	if _, err := src.Proposals(ctx, "solo"); !errors.Is(err, tui.ErrMissing) {
		t.Errorf("a ticket without a contract: %v", err)
	}
}

func TestTheTUIPushesAReadyTicket(t *testing.T) {
	home, d, _ := newEnv(t)
	r1, bare1 := repoWithRemote(t)
	pushReady(t, d, home, []string{"push-branch"}, r1)
	res, err := source(d).Push(context.Background(), "demo")
	if err != nil || len(res.Services) != 1 || !res.Services[0].Pushed || !res.Done {
		t.Fatalf("push = %+v, %v", res, err)
	}
	if out := testkit.RunGit(t, bare1, "branch", "--list"); !strings.Contains(out, "rig/demo") {
		t.Errorf("the branch did not reach the remote: %q", out)
	}
}
