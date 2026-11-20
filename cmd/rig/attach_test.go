package main

import (
	"context"
	"fmt"

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

func TestStartRunsAContainerServicesAgentInItsOwnContainer(t *testing.T) {
	_, d, repo, term, dk := newEnvFull(t)
	newTicket(t, d, "demo", repo)
	stdout, stderr, code := rig(t, d, "start", "demo")
	if code != 0 || !strings.Contains(stdout, "agent svc1: started") {
		t.Fatalf("code %d, stdout:\n%s\nstderr %q", code, stdout, stderr)
	}
	if c, err := dk.InspectContainer(ctx, "rig-demo-svc1-agent"); err != nil || c.Labels["rig.role"] != "agent" {
		t.Errorf("agent container = %+v, %v", c, err)
	}
	spawned := term.Spawned()
	if len(spawned) != 1 || strings.Join(spawned[0].Command[:3], " ") != "docker exec -it" || !strings.Contains(strings.Join(spawned[0].Command, " "), "rig-demo-svc1-agent claude") {
		t.Errorf("pane command = %q, want a docker exec into the agent container", spawned)
	}
	if strings.Contains(stdout+stderr, "WARNING") {
		t.Errorf("a sandboxed agent must not print the host warning:\n%s%s", stdout, stderr)
	}
}

func TestStartWarnsLoudlyWhenAnAgentRunsOnTheHost(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	stdout, stderr, code := rig(t, d, "start", "demo", "--sandbox", "host")
	if code != 0 || !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, "svc1") || !strings.Contains(stderr, "directly on this machine") {
		t.Errorf("code %d, stderr %q, stdout %q", code, stderr, stdout)
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

func TestAttachReadShowsTheScreenWithoutTouchingThePane(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	panes, _ := term.List(ctx)
	term.Print(panes[0].ID, "line one\nline two\nline three\n")
	d.exec = func([]string) error { t.Error("--read ran the attach command"); return nil }

	stdout, stderr, code := rig(t, d, "attach", "demo", "--read")
	if code != 0 || !strings.Contains(stdout, "line one") || !strings.Contains(stdout, "line three") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stdout, _, _ = rig(t, d, "attach", "demo", "svc1", "--read", "--lines", "1")
	if strings.Contains(stdout, "line two") || !strings.Contains(stdout, "line three") {
		t.Errorf("--lines 1: %q", stdout)
	}
	stdout, _, _ = rig(t, d, "attach", "demo", "--read", "--json")
	var res api.AgentAttachResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || !strings.Contains(res.Screen, "line two") {
		t.Errorf("--read --json: %q, %v", stdout, err)
	}
	stdout, _, _ = rig(t, d, "attach", "demo", "--json")
	if strings.Contains(stdout, "screen") {
		t.Errorf("attach without --read carries the screen: %q", stdout)
	}
	if in := term.Input(panes[0].ID); len(in) != 0 {
		t.Errorf("reading typed into the pane: %v", in)
	}
	if _, stderr, code := rig(t, d, "attach", "demo", "--lines", "3"); code != 2 || !strings.Contains(stderr, "--read") {
		t.Errorf("--lines without --read: code %d, %q", code, stderr)
	}
}

func TestAttachReadPagesTheScrollbackInATerminal(t *testing.T) {
	_, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	panes, _ := term.List(ctx)
	var screen strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&screen, "line %d\n", i)
	}
	term.Print(panes[0].ID, screen.String())
	d.isTerminal = func() bool { return true }
	var paged []string
	d.page = func(text string) error { paged = append(paged, text); return nil }

	stdout, stderr, code := rig(t, d, "attach", "demo", "--read")
	if code != 0 || len(paged) != 1 || stdout != "" {
		t.Fatalf("code %d, paged %d times, stdout %q, stderr %q", code, len(paged), stdout, stderr)
	}
	if !strings.Contains(paged[0], "line 2500") || !strings.Contains(paged[0], "line 501\n") || strings.Contains(paged[0], "line 500\n") {
		t.Errorf("the pager did not get the last 2000 lines")
	}

	paged = nil
	rig(t, d, "attach", "demo", "--read", "--lines", "3")
	if len(paged) != 1 || strings.Count(paged[0], "line") != 3 {
		t.Errorf("--lines 3 paged %q", paged)
	}
	paged = nil
	if stdout, _, _ := rig(t, d, "attach", "demo", "--read", "--json"); len(paged) != 0 || !strings.Contains(stdout, `line 1\n`) {
		t.Errorf("--json paged (%d) or did not carry the whole screen", len(paged))
	}
	paged = nil
	if stdout, _, _ := rig(t, d, "attach", "demo", "--read", "--no-pager"); len(paged) != 0 || !strings.Contains(stdout, "line 2500") {
		t.Errorf("--no-pager paged (%d) or printed nothing", len(paged))
	}
}

func TestAttachReadSaysWhenTheScreenIsEmpty(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	if _, stderr, code := rig(t, d, "attach", "demo", "--read"); code != 0 || !strings.Contains(stderr, "rig daemon stop") {
		t.Errorf("code %d, stderr %q", code, stderr)
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
		{"no agent yet, read", []string{"attach", "demo", "--read"}, 4, "start the ticket"},
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
