package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

func TestTheReviewLoopRunsThroughTheCLI(t *testing.T) {
	home, d, repo, term, _ := newEnvFull(t)
	newTicket(t, d, "demo", repo)
	yaml := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yaml)
	os.WriteFile(yaml, append(raw, []byte("review: {max_rounds: 2}\n")...), 0o600)
	if _, stderr, code := rig(t, d, "start", "demo"); code != 0 {
		t.Fatalf("start: %s", stderr)
	}
	rig(t, d, "approve", "demo", "plan_approval")
	rig(t, d, "phase", "demo", "implement")

	coder := filepath.Join(home, "tickets", "demo", "svc1")
	os.WriteFile(filepath.Join(coder, "a.txt"), []byte("work\n"), 0o644)
	for _, args := range [][]string{{"add", "a.txt"}, {"-c", "user.name=c", "-c", "user.email=c@example.com", "commit", "-q", "-m", "work"}} {
		if _, err := git.Hardened().Run(ctx, coder, args...); err != nil {
			t.Fatal(err)
		}
	}
	status := func() api.ReviewerStatus {
		stdout, _, _ := rig(t, d, "reviewer", "demo", "--json")
		var s api.ReviewerStatus
		json.Unmarshal([]byte(stdout), &s)
		return s
	}

	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"review.ready","text":"a.txt is done"}`)
	waitFor(t, "round 1 to start", func() bool { s := status(); return s.Status == domain.ReviewReviewing && s.Commit != "" })

	t.Setenv("RIG_SERVICE", "review")
	rig(t, d, "agent-event", `{"kind":"review.result","text":"empty","data":{"id":"F1","severity":"blocking","file":"a.txt","line":1,"what":"no newline handling","scenario":"empty file","fix":"handle it"}}`)
	rig(t, d, "agent-event", `{"kind":"verdict","text":"Blocking findings: 1","data":{"round":1}}`)
	if s := status(); s.Status != domain.ReviewFixing {
		t.Fatalf("status = %+v, want fixing", s)
	}
	stdout, _, _ := rig(t, d, "reviewer", "demo")
	for _, want := range []string{"fixing, round 1 of at most 2", "F1 blocking", "a.txt:1 no newline handling", "-> no answer"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("rig reviewer lacks %q:\n%s", want, stdout)
		}
	}
	if stdout, _, _ := rig(t, d, "list"); !strings.Contains(stdout, "review fixing r1") {
		t.Errorf("list:\n%s", stdout)
	}

	// The findings are typed into the coder's pane.
	panes, _ := term.List(ctx)
	var coderPane terminal.PaneID
	for _, p := range panes {
		if p.Service == "svc1" {
			coderPane = p.ID
		}
	}
	rig(t, d, "agent-event", "idle")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", "idle")
	waitFor(t, "the findings in the coder's pane", func() bool {
		for _, in := range term.Input(coderPane) {
			if strings.Contains(in.Text, "F1 [blocking] a.txt:1") {
				return true
			}
		}
		return false
	})

	if _, stderr, code := rig(t, d, "phase", "demo", "test"); code != 3 || !strings.Contains(stderr, "review") {
		t.Errorf("phase before a pass: code %d, %q", code, stderr)
	}
	if _, stderr, code := rig(t, d, "reviewer", "demo", "--pass"); code != 2 || !strings.Contains(stderr, "--reason") {
		t.Errorf("--pass without --reason: code %d, %q", code, stderr)
	}
	if stdout, stderr, code := rig(t, d, "reviewer", "demo", "--pass", "--reason", "accepted as is"); code != 0 || !strings.Contains(stdout, "passed") {
		t.Fatalf("pass: code %d, %q %q", code, stdout, stderr)
	}
	if _, stderr, code := rig(t, d, "phase", "demo", "test"); code != 0 {
		t.Errorf("phase after a pass: %s", stderr)
	}

	if _, stderr, code := rig(t, d, "stop", "demo"); code != 0 {
		t.Fatalf("stop: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "tickets", "demo", "review")); !os.IsNotExist(err) {
		t.Error("the reviewer's copy survived stop")
	}
}

func TestReviewerErrors(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	tests := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"no review configured", []string{"reviewer", "demo"}, 2, "no review"},
		{"two actions", []string{"reviewer", "demo", "--start", "--pass", "--reason", "x"}, 2, "one of"},
		{"a reason alone", []string{"reviewer", "demo", "--reason", "x"}, 2, "--reason goes with"},
		{"unknown ticket", []string{"reviewer", "ghost"}, 5, "ghost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stderr, code := rig(t, d, tt.args...)
			if code != tt.code || !strings.Contains(stderr, tt.msg) {
				t.Errorf("code %d (want %d), stderr %q (want %q)", code, tt.code, stderr, tt.msg)
			}
		})
	}
}

func TestTheTesterIsShownPerRoundThroughTheCLI(t *testing.T) {
	home, d, repo, _, _ := newEnvFull(t)
	newTicket(t, d, "demo", repo)
	yaml := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yaml)
	os.WriteFile(yaml, append(raw, []byte("review: {tester: {}}\n")...), 0o600)
	if _, stderr, code := rig(t, d, "start", "demo"); code != 0 {
		t.Fatalf("start: %s", stderr)
	}
	rig(t, d, "approve", "demo", "plan_approval")
	rig(t, d, "phase", "demo", "implement")
	coder := filepath.Join(home, "tickets", "demo", "svc1")
	os.WriteFile(filepath.Join(coder, "a.txt"), []byte("work\n"), 0o644)
	for _, args := range [][]string{{"add", "a.txt"}, {"-c", "user.name=c", "-c", "user.email=c@example.com", "commit", "-q", "-m", "work"}} {
		if _, err := git.Hardened().Run(ctx, coder, args...); err != nil {
			t.Fatal(err)
		}
	}
	status := func() api.ReviewerStatus {
		stdout, _, _ := rig(t, d, "reviewer", "demo", "--json")
		var s api.ReviewerStatus
		json.Unmarshal([]byte(stdout), &s)
		return s
	}
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"review.ready","text":"done"}`)
	waitFor(t, "round 1 to start", func() bool { s := status(); return s.Status == domain.ReviewReviewing && s.Commit != "" })

	t.Setenv("RIG_SERVICE", "review")
	rig(t, d, "agent-event", `{"kind":"verdict","text":"No blocking findings","data":{"round":1}}`)
	if stdout, _, _ := rig(t, d, "reviewer", "demo"); !strings.Contains(stdout, "tester: no result yet") {
		t.Errorf("before the tester answers:\n%s", stdout)
	}
	t.Setenv("RIG_SERVICE", "tester")
	rig(t, d, "agent-event", `{"kind":"test.result","text":"broke","data":{"outcome":"fail","findings":[{"severity":"blocking","text":"crash on empty input"}]}}`)
	stdout, _, _ := rig(t, d, "reviewer", "demo")
	for _, want := range []string{"fixing, round 1", "tester: fail, 1 findings", "T1 blocking", "crash on empty input"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("rig reviewer lacks %q:\n%s", want, stdout)
		}
	}
	if s := status(); s.Rounds[0].Tester == nil || s.Rounds[0].Tester.Outcome != "fail" {
		t.Errorf("--json status = %+v", s.Rounds)
	}
	if stdout, _, _ := rig(t, d, "timeline", "demo"); !strings.Contains(stdout, "test.result") {
		t.Errorf("timeline:\n%s", stdout)
	}
}
