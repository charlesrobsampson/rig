package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func agentState(t *testing.T, d deps, ticket string) domain.AgentState {
	t.Helper()
	stdout, _, code := rig(t, d, "list", "--json")
	var res api.TicketListResult
	if err := json.Unmarshal([]byte(stdout), &res); code != 0 || err != nil {
		t.Fatalf("list: code %d, %q, %v", code, stdout, err)
	}
	for _, tk := range res.Tickets {
		if tk.ID == ticket {
			return tk.Services[0].AgentState
		}
	}
	t.Fatalf("ticket %s not listed", ticket)
	return ""
}

func TestAgentEventUpdatesTheServicesState(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")

	for _, st := range []string{"working", "blocked", "idle"} {
		stdout, stderr, code := rig(t, d, "agent-event", st)
		if code != 0 || stdout != "" || stderr != "" {
			t.Fatalf("agent-event %s: code %d, stdout %q, stderr %q; want a silent success", st, code, stdout, stderr)
		}
		if got := agentState(t, d, "demo"); string(got) != st {
			t.Errorf("after %s the state is %s", st, got)
		}
	}
}

// Hooks treat a failing command as "block this action", so nothing may fail.
func TestAgentEventNeverFailsTheAgent(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	tests := []struct {
		name, ticket, service string
		args                  []string
	}{
		{"no arguments", "demo", "svc1", nil},
		{"too many arguments", "demo", "svc1", []string{"idle", "extra"}},
		{"an unknown state", "demo", "svc1", []string{"sleeping"}},
		{"an unknown ticket", "nope", "svc1", []string{"idle"}},
		{"an unknown service", "demo", "nope", []string{"idle"}},
		{"no ticket in the environment", "", "svc1", []string{"idle"}},
		{"no service in the environment", "demo", "", []string{"idle"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RIG_TICKET", tt.ticket)
			t.Setenv("RIG_SERVICE", tt.service)
			stdout, stderr, code := rig(t, d, append([]string{"agent-event"}, tt.args...)...)
			if code != 0 || stdout != "" || stderr != "" {
				t.Errorf("code %d, stdout %q, stderr %q; want silent exit 0", code, stdout, stderr)
			}
		})
	}
	if got := agentState(t, d, "demo"); got != domain.AgentUnknown && got != "" {
		t.Errorf("bad reports changed the state to %s", got)
	}
}

func TestAgentEventWithNoDaemonExitsZeroQuicklyAndStartsNothing(t *testing.T) {
	home := rigEnv(t)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	started := false
	d := deps{spawner: func(context.Context) error { started = true; return nil }}

	begin := time.Now()
	stdout, stderr, code := rig(t, d, "agent-event", "idle")
	if code != 0 || stdout != "" || stderr != "" {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if time.Since(begin) > 2*time.Second {
		t.Errorf("took %s; a hook must not hold the agent up", time.Since(begin))
	}
	if started {
		t.Errorf("a hook report started a daemon (home %s)", home)
	}
}
