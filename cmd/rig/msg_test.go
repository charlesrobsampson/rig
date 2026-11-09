package main

import (
	"strings"
	"testing"
)

func TestMsgReachesTheAgentAndRefusesWhenBlocked(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", "ready")
	panes, _ := term.List(ctx)

	stdout, stderr, code := rig(t, d, "msg", "demo", "svc1", "please", "rerun", "the", "tests")
	if code != 0 || !strings.Contains(stdout, "sent to demo/svc1") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	input := term.Input(panes[0].ID)
	if len(input) != 2 || input[0].Text != "[rig] please rerun the tests" || !input[1].Submit {
		t.Errorf("input = %v", input)
	}

	rig(t, d, "agent-event", "blocked")
	_, stderr, code = rig(t, d, "msg", "demo", "svc1", "again")
	if code != 3 || !strings.Contains(stderr, "permission prompt") {
		t.Errorf("code %d (want 3, blocked), stderr %q", code, stderr)
	}
	if got := term.Input(panes[0].ID); len(got) != 2 {
		t.Errorf("a refused message typed something: %v", got)
	}
}

func TestMsgReadsStdinAndReportsErrors(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", "idle")
	panes, _ := term.List(ctx)

	d.stdin = strings.NewReader("from\nstdin\n")
	if _, stderr, code := rig(t, d, "msg", "demo", "svc1", "-"); code != 0 {
		t.Fatalf("code %d, %q", code, stderr)
	}
	if got := term.Input(panes[0].ID); len(got) != 2 || got[0].Text != "[rig] from\nstdin" {
		t.Errorf("input = %v", got)
	}

	tests := []struct {
		name string
		args []string
		code int
	}{
		{"an unknown ticket", []string{"msg", "ghost", "svc1", "x"}, 5},
		{"an unknown service", []string{"msg", "demo", "nope", "x"}, 5},
		{"too few arguments", []string{"msg", "demo", "svc1"}, 2},
	}
	for _, tt := range tests {
		if _, _, code := rig(t, d, tt.args...); code != tt.code {
			t.Errorf("%s: code %d, want %d", tt.name, code, tt.code)
		}
	}
}
