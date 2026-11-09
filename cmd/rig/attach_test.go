package main

import (
	"context"

	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestStartLaunchesAgentsAndListShowsTheirState(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	other := testkit.NewRepo(t)
	newTicket(t, d, "demo", repo, other)

	stdout, stderr, code := rig(t, d, "start", "demo", "--sandbox", "host")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"agent svc1: started", "agent svc2: started"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if len(term.Spawned()) != 2 {
		t.Errorf("spawned %d panes", len(term.Spawned()))
	}

	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", "working")
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "svc1:provisioned/working") || strings.Contains(listing, "svc2:provisioned/") {
		t.Errorf("list:\n%s", listing)
	}
}

func TestStartExplainsWhyAContainerServiceGotNoAgent(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	stdout, _, code := rig(t, d, "start", "demo")
	if code != 0 || !strings.Contains(stdout, "agent svc1: no agent started") || len(term.Spawned()) != 0 {
		t.Errorf("code %d, stdout:\n%s", code, stdout)
	}
}

func TestStartFailsLoudlyWhenAnAgentCannotStart(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	term.Down(terminal.ErrUnavailable)
	stdout, stderr, code := rig(t, d, "start", "demo", "--sandbox", "host")
	if code != 1 || !strings.Contains(stdout, "agent svc1: failed") || !strings.Contains(stderr, "did not start") {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "svc1:provisioned") {
		t.Errorf("the checkout should still be provisioned:\n%s", listing)
	}
}

func TestAttachRunsTheBackendsCommand(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")

	var ran []string
	d.exec = func(argv []string) error { ran = argv; return nil }
	if _, stderr, code := rig(t, d, "attach", "demo", "svc1"); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	panes, _ := term.List(ctx)
	want, _ := term.Attach(ctx, panes[0].ID)
	if strings.Join(ran, " ") != strings.Join(want, " ") || len(ran) == 0 {
		t.Errorf("ran %v, want %v", ran, want)
	}

	ran = nil
	if _, _, code := rig(t, d, "attach", "demo"); code != 0 || len(ran) == 0 {
		t.Errorf("a ticket with one service needs no service name: code %d, ran %v", code, ran)
	}

	ran = nil
	stdout, _, code := rig(t, d, "attach", "demo", "--print")
	if code != 0 || strings.TrimSpace(stdout) != strings.Join(want, " ") || ran != nil {
		t.Errorf("--print: code %d, stdout %q, ran %v", code, stdout, ran)
	}

	stdout, _, _ = rig(t, d, "attach", "demo", "--json")
	var res api.AgentAttachResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || res.Service != "svc1" || len(res.Command) == 0 {
		t.Errorf("--json: %q, %v", stdout, err)
	}
}

func TestAttachErrors(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	d.exec = func([]string) error { t.Error("attach ran a command for a failed lookup"); return nil }
	tests := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"no agent yet", []string{"attach", "demo"}, 4, "start the ticket"},
		{"an unknown service", []string{"attach", "demo", "nope"}, 5, "nope"},
		{"an unknown ticket", []string{"attach", "ghost"}, 5, "ghost"},
		{"no arguments", []string{"attach"}, 2, ""},
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

func TestStopClosesTheAgentPanes(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	if panes, _ := term.List(ctx); len(panes) != 1 {
		t.Fatalf("panes = %v", panes)
	}
	if _, stderr, code := rig(t, d, "stop", "demo"); code != 0 {
		t.Fatalf("stop: code %d, %q", code, stderr)
	}
	if panes, _ := term.List(ctx); len(panes) != 0 {
		t.Errorf("panes after stop: %v", panes)
	}
}

var ctx = context.Background()
