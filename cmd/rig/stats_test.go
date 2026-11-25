package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/state"
)

func TestStatsAndTheListShowWhatATicketUsed(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	dir := filepath.Join(home, "tickets", "demo")
	state.NewManager(clock.Real()).Update(filepath.Join(dir, "state.json"), func(s *state.State) error {
		ss := s.Services["svc1"]
		ss.Agent.Cost = &state.Cost{USD: 0.5}
		s.Services["svc1"] = ss
		s.Services["review"] = state.ServiceState{Agent: state.Agent{Cost: &state.Cost{USD: 0.25}}}
		return nil
	})
	transcript := `{"type":"assistant","timestamp":"2026-11-25T14:00:00Z","requestId":"r","message":{"id":"m","model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":20000,"cache_read_input_tokens":1900000,"output_tokens":24000}}}` + "\n"
	p := filepath.Join(dir, ".rig-claude", "svc1", "projects", "-work", "s.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(transcript), 0o600)

	stdout, stderr, code := rig(t, d, "list")
	if code != 0 || !strings.Contains(stdout, "COST") || !strings.Contains(stdout, "TIME") || !strings.Contains(stdout, "$0.75") {
		t.Errorf("list: %d\n%s%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "$0.50") {
		t.Errorf("the list still shows a per-service cost:\n%s", stdout)
	}

	stdout, stderr, code = rig(t, d, "stats")
	if code != 0 || !strings.Contains(stdout, "1.9M") || !strings.Contains(stdout, "24k") || !strings.Contains(stdout, "TOTAL") || !strings.Contains(stdout, "$0.75") {
		t.Errorf("stats: %d\n%s%s", code, stdout, stderr)
	}
	stdout, _, code = rig(t, d, "stats", "demo")
	for _, want := range []string{"cost    $0.75", "cache read 1.9M", "coder", "claude-sonnet-5-5"} {
		if code != 0 || !strings.Contains(stdout, want) {
			t.Errorf("stats demo lacks %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = rig(t, d, "stats", "--json")
	var res api.StatsResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || res.Totals.Tickets != 1 || res.Totals.Tokens.CacheRead != 1900000 || res.Totals.CostUSD != 0.75 {
		t.Errorf("stats --json: %v\n%s", err, stdout)
	}
	if _, _, code := rig(t, d, "stats", "nope"); code == 0 {
		t.Error("stats for an unknown ticket succeeded")
	}
}
