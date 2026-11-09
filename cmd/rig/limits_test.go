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

func TestTheLimitOnActiveTicketsIsEnforcedAndConfigurable(t *testing.T) {
	_, d, repo := newEnv(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"limits":{"max_active_tickets":1}}`), 0o600)
	t.Setenv("RIG_CONFIG", cfg)
	newTicket(t, d, "first", repo)
	newTicket(t, d, "second", testkit.NewRepo(t))

	if _, stderr, code := rig(t, d, "start", "first", "--sandbox", "host"); code != 0 {
		t.Fatalf("first: code %d, %q", code, stderr)
	}
	_, stderr, code := rig(t, d, "start", "second", "--sandbox", "host")
	if code != 3 || !strings.Contains(stderr, "1 tickets are already active and the limit is 1") || !strings.Contains(stderr, "active: first") || !strings.Contains(stderr, "rig stop") {
		t.Errorf("second: code %d (want 3, blocked), stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("RIG_HOME"), "tickets", "second", "svc1")); err == nil {
		t.Error("the refused start provisioned a checkout")
	}
	// the one that is active can still be started again
	if _, stderr, code := rig(t, d, "start", "first", "--sandbox", "host"); code != 0 {
		t.Errorf("restarting the active ticket: code %d, %q", code, stderr)
	}
	// stopping frees the slot
	if _, stderr, code := rig(t, d, "stop", "first", "--yes"); code != 0 {
		t.Fatalf("stop: %q", stderr)
	}
	if _, stderr, code := rig(t, d, "start", "second", "--sandbox", "host"); code != 0 {
		t.Errorf("after stopping: code %d, %q", code, stderr)
	}
}

func TestAnAgentsCostArrivesThroughItsStatusLineAndShowsInList(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")

	// before any report there is no cost, in the JSON or the table
	out, _, _ := rig(t, d, "list", "--json")
	if strings.Contains(out, "cost_usd") {
		t.Errorf("an unreported cost appears in list --json: %s", out)
	}

	d.stdin = strings.NewReader(`{"session_id":"s","cost":{"total_cost_usd":1.2345,"total_duration_ms":10}}`)
	stdout, stderr, code := rig(t, d, "agent-event", "cost")
	if code != 0 || stdout != "$1.23\n" || stderr != "" {
		t.Errorf("status line: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	var res api.TicketListResult
	out, _, _ = rig(t, d, "list", "--json")
	json.Unmarshal([]byte(out), &res)
	if c := res.Tickets[0].Services[0].CostUSD; c == nil || *c != 1.2345 {
		t.Errorf("list --json cost = %v", c)
	}
	if table, _, _ := rig(t, d, "list"); !strings.Contains(table, "$1.23") {
		t.Errorf("list table:\n%s", table)
	}

	// a status with no cost, or no JSON at all, reports nothing, prints nothing, and still exits 0
	for _, in := range []string{`{"session_id":"s"}`, `not json`, ``, `{"cost":null}`, `{"cost":{}}`} {
		d.stdin = strings.NewReader(in)
		if stdout, _, code := rig(t, d, "agent-event", "cost"); code != 0 || stdout != "" {
			t.Errorf("input %q: code %d, stdout %q", in, code, stdout)
		}
	}
	if out, _, _ := rig(t, d, "list", "--json"); !strings.Contains(out, "1.2345") {
		t.Error("a status update without a cost erased the cost")
	}
	// with no daemon the hook still exits 0 quietly
	t.Setenv("RIG_TICKET", "")
	d.stdin = strings.NewReader(`{"cost":{"total_cost_usd":5}}`)
	if stdout, _, code := rig(t, d, "agent-event", "cost"); code != 0 || stdout != "" {
		t.Errorf("without RIG_TICKET: code %d, stdout %q", code, stdout)
	}
}

func TestHostAgentsAreLaunchedWithTheCostStatusLine(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	out, _, code := rig(t, d, "start", "demo", "--sandbox", "host", "--dry-run", "--json")
	if code != 0 || !strings.Contains(out, `statusLine`) || !strings.Contains(out, `agent-event cost`) {
		t.Errorf("code %d, the launch lacks the status line:\n%s", code, out)
	}
}
