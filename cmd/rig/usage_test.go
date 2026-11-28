package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func usageFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	put := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := func(ts, id, model string, in, cw, cr, out int) string {
		return `{"type":"assistant","timestamp":"` + ts + `","requestId":"r` + id + `","message":{"id":"m` + id + `","model":"` + model + `","usage":{"input_tokens":` + itoa(in) + `,"cache_creation_input_tokens":` + itoa(cw) + `,"cache_read_input_tokens":` + itoa(cr) + `,"output_tokens":` + itoa(out) + `}}}` + "\n"
	}
	// a request repeated on a second line, and again in a sub-agent file
	put("projects/-work/s1.jsonl", req("2026-11-25T14:00:00Z", "1", "claude-sonnet-5-5", 10, 1000, 5000, 200)+req("2026-11-25T14:00:01Z", "1", "claude-sonnet-5-5", 10, 1000, 5000, 200))
	put("projects/-work/s1/subagents/a.jsonl", req("2026-11-25T14:00:02Z", "1", "claude-sonnet-5-5", 10, 1000, 5000, 200)+req("2026-11-25T14:05:00Z", "2", "claude-haiku-5-5", 1, 0, 0, 9))
	put("projects/-play/s2.jsonl", req("2026-12-03T09:00:00Z", "3", "claude-opus-5-5", 5, 0, 6000, 50))
	return dir
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestUsageNeedsNoDaemonAndPricesEachSession(t *testing.T) {
	rigEnv(t)
	dir := usageFixture(t)
	stdout, stderr, code := rig(t, deps{}, "usage", dir)
	if code != 0 {
		t.Fatalf("code %d\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{dir, "projects/-work/s1.jsonl", "projects/-work/s1/subagents/a.jsonl", "projects/-play/s2.jsonl", "TOTAL", "claude-opus-5-5", "prices taken 2026-11-03"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "unpriced") {
		t.Errorf("nothing is unpriced:\n%s", stdout)
	}
}

func TestUsageJSONShapeSinceAndDuplicates(t *testing.T) {
	rigEnv(t)
	dir := usageFixture(t)
	var rep usageReport
	stdout, _, code := rig(t, deps{}, "usage", dir, "--json")
	if code != 0 || json.Unmarshal([]byte(stdout), &rep) != nil {
		t.Fatalf("code %d\n%s", code, stdout)
	}
	d := rep.Directories[0]
	if d.Dir != dir || len(d.Sessions) != 3 || d.Requests != 3 || d.Tokens.Output != 200+9+50 || d.Tokens.CacheWrite != 1000 {
		t.Errorf("directory: %+v", d)
	}
	sonnet := (10*2 + 1000*2*1.25 + 5000*0.10 + 200*10) / 1e6
	haiku := (1*0.10 + 9*0.50) / 1e6
	opus := (5*4 + 6000*0.20 + 50*20) / 1e6
	if math.Abs(d.CostUSD-(sonnet+haiku+opus)) > 1e-12 || math.Abs(rep.Totals.CostUSD-d.CostUSD) > 1e-12 {
		t.Errorf("cost %v, want %v", d.CostUSD, sonnet+haiku+opus)
	}
	if d.First == nil || d.First.Format("2006-01-02T15:04:05Z") != "2026-11-25T14:00:00Z" || rep.Prices.Taken != "2026-11-03" || rep.Since != "" {
		t.Errorf("first/prices: %+v %+v", d.First, rep.Prices)
	}
	// 14:00:00 -> :01 -> :02, then 3 minutes
	for _, s := range d.Sessions {
		if s.File == "projects/-work/s1.jsonl" && (s.Requests != 1 || s.AgentSeconds != 1) {
			t.Errorf("session s1: %+v", s)
		}
	}
	if d.AgentSeconds != 2+298+0 {
		t.Errorf("agent seconds %d", d.AgentSeconds)
	}

	stdout, _, code = rig(t, deps{}, "usage", dir, "--json", "--since", "2026-12-01T00:00:00Z")
	var late usageReport
	if code != 0 || json.Unmarshal([]byte(stdout), &late) != nil || late.Totals.Requests != 1 || len(late.Directories[0].Sessions) != 1 || late.Since != "2026-12-01T00:00:00Z" {
		t.Errorf("since: %d\n%s", code, stdout)
	}
	if _, _, code := rig(t, deps{}, "usage", dir, "--since", "yesterday-ish"); code != 2 {
		t.Errorf("a bad --since exits %d, want 2", code)
	}
}

func TestUsageNamesADirectoryThatIsNotATranscriptsFolder(t *testing.T) {
	rigEnv(t)
	good, bad := usageFixture(t), t.TempDir()
	stdout, stderr, code := rig(t, deps{}, "usage", good, bad)
	if code != 2 || !strings.Contains(stderr, bad) || stdout != "" {
		t.Errorf("code %d\n%s%s", code, stdout, stderr)
	}
	if _, _, code := rig(t, deps{}, "usage"); code != 2 {
		t.Errorf("no directory exits %d, want 2", code)
	}
}

func TestUsageListsAnUnpricedModelAndMarksTheCostALowerBound(t *testing.T) {
	rigEnv(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "projects", "p"), 0o700)
	os.WriteFile(filepath.Join(dir, "projects", "p", "s.jsonl"), []byte(`{"type":"assistant","timestamp":"2026-11-25T14:00:00Z","requestId":"r","message":{"id":"m","model":"claude-mystery-9","usage":{"input_tokens":7,"output_tokens":3}}}`+"\n"), 0o600)
	stdout, _, code := rig(t, deps{}, "usage", dir)
	if code != 0 || !strings.Contains(stdout, "$0.00+") || !strings.Contains(stdout, "claude-mystery-9") || !strings.Contains(stdout, "unpriced") {
		t.Errorf("%d\n%s", code, stdout)
	}
	var rep usageReport
	out, _, _ := rig(t, deps{}, "usage", dir, "--json")
	if json.Unmarshal([]byte(out), &rep) != nil || len(rep.Totals.Unpriced) != 1 || rep.Directories[0].Tokens.Input != 7 {
		t.Errorf("json: %s", out)
	}
}

func TestUsageAgreesWithStatsOnATicketsTranscripts(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	cfg := filepath.Join(home, "tickets", "demo", ".rig-claude", "svc1")
	line := func(ts, id string, cr int) string {
		return `{"type":"assistant","timestamp":"` + ts + `","requestId":"r` + id + `","message":{"id":"m` + id + `","model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":20000,"cache_read_input_tokens":` + itoa(cr) + `,"output_tokens":24000}}}` + "\n"
	}
	p := filepath.Join(cfg, "projects", "-work", "s.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(line("2026-11-25T14:00:00Z", "1", 1900000)+line("2026-11-25T14:00:00Z", "1", 1900000)+line("2026-11-25T14:04:00Z", "2", 10)), 0o600)

	var stats struct {
		Totals struct {
			Tokens struct {
				Input      int64 `json:"input"`
				CacheWrite int64 `json:"cache_write"`
				CacheRead  int64 `json:"cache_read"`
				Output     int64 `json:"output"`
			}
			AgentSeconds int64 `json:"agent_seconds"`
		}
	}
	out, _, _ := rig(t, d, "stats", "--json")
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatal(err)
	}
	var rep usageReport
	out, _, code := rig(t, deps{}, "usage", cfg, "--json")
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
		t.Fatalf("%d %s", code, out)
	}
	tk := rep.Totals.Tokens
	if tk.Input != stats.Totals.Tokens.Input || tk.CacheWrite != stats.Totals.Tokens.CacheWrite || tk.CacheRead != stats.Totals.Tokens.CacheRead || tk.Output != stats.Totals.Tokens.Output || rep.Totals.Requests != 2 || rep.Totals.AgentSeconds != stats.Totals.AgentSeconds || stats.Totals.AgentSeconds != 240 {
		t.Errorf("usage %+v, stats %+v", rep.Totals, stats.Totals)
	}
}
