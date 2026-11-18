package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
)

func journalJSON(t *testing.T, d deps, args ...string) []api.JournalRecord {
	t.Helper()
	stdout, stderr, code := rig(t, d, append([]string{"journal", "--json"}, args...)...)
	var res api.JournalListResult
	if err := json.Unmarshal([]byte(stdout), &res); code != 0 || err != nil {
		t.Fatalf("journal --json: code %d, %q, %q, %v", code, stdout, stderr, err)
	}
	return res.Records
}

func TestAHostAgentRecordsThroughAgentEvent(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")

	stdout, stderr, code := rig(t, d, "agent-event", `{"kind":"feedback","text":"gh is not logged in","data":{"tool":"gh"}}`)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("code %d, stdout %q, stderr %q; want a silent success", code, stdout, stderr)
	}
	got := journalJSON(t, d, "demo")
	var data bytes.Buffer
	if len(got) == 1 {
		json.Compact(&data, got[0].Data)
	}
	if len(got) != 1 || got[0].Kind != "feedback" || got[0].Text != "gh is not logged in" || got[0].Source != api.JournalSourceHost || got[0].Service != "svc1" || data.String() != `{"tool":"gh"}` {
		t.Errorf("journal = %+v", got)
	}
}

// A bad record must not fail the agent's command either: hooks and agents run it.
func TestABadRecordThroughAgentEventIsSilentAndJournalsNothing(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	for _, arg := range []string{`{broken`, `{"kind":"Bad"}`, `{"text":"no kind"}`} {
		stdout, stderr, code := rig(t, d, "agent-event", arg)
		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%s: code %d, stdout %q, stderr %q", arg, code, stdout, stderr)
		}
	}
	if got := journalJSON(t, d, "demo"); len(got) != 0 {
		t.Errorf("journal = %+v", got)
	}
}

func TestRigJournalFiltersAndPrints(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "one", repo)
	newTicket(t, d, "two", repo)
	for _, r := range []struct{ ticket, line string }{
		{"one", `{"kind":"feedback","text":"first line\nsecond line"}`},
		{"two", `{"kind":"verdict","text":"PASS"}`},
		{"two", `{"kind":"feedback","text":"proxy refused registry.npmjs.org"}`},
	} {
		t.Setenv("RIG_TICKET", r.ticket)
		t.Setenv("RIG_SERVICE", "svc1")
		rig(t, d, "agent-event", r.line)
	}

	if got := journalJSON(t, d); len(got) != 3 {
		t.Errorf("all tickets: %d records", len(got))
	}
	if got := journalJSON(t, d, "--kind", "feedback"); len(got) != 2 {
		t.Errorf("feedback: %+v", got)
	}
	if got := journalJSON(t, d, "two", "--limit", "1"); len(got) != 1 || got[0].Text != "proxy refused registry.npmjs.org" {
		t.Errorf("newest of two: %+v", got)
	}
	if got := journalJSON(t, d, "one", "--service", "svc2"); len(got) != 0 {
		t.Errorf("other service: %+v", got)
	}

	stdout, _, code := rig(t, d, "journal", "--kind", "feedback")
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if code != 0 || len(lines) != 3 || !strings.Contains(lines[0], "KIND") || !strings.Contains(lines[1], "one") || !strings.Contains(lines[1], "first line") || strings.Contains(stdout, "second line") || !strings.Contains(lines[2], "proxy refused") {
		t.Errorf("table (code %d):\n%s", code, stdout)
	}

	stdout, _, code = rig(t, d, "journal", "one", "--kind", "none")
	if code != 0 || strings.TrimSpace(stdout) != "no records" {
		t.Errorf("empty: code %d, %q", code, stdout)
	}
	if _, _, code := rig(t, d, "journal", "nope"); code != 5 {
		t.Errorf("unknown ticket exits %d, want 5", code)
	}
	if _, _, code := rig(t, d, "journal", "--limit", "-1"); code != 2 {
		t.Errorf("negative limit exits %d, want 2", code)
	}
}
