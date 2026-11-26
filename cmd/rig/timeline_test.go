package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
)

func timelineJSON(t *testing.T, d deps, args ...string) []api.TimelineEntry {
	t.Helper()
	stdout, stderr, code := rig(t, d, append([]string{"timeline", "--json"}, args...)...)
	var res api.TimelineListResult
	if err := json.Unmarshal([]byte(stdout), &res); code != 0 || err != nil {
		t.Fatalf("timeline --json: code %d, %q, %q, %v", code, stdout, stderr, err)
	}
	return res.Entries
}

func TestTimelineShowsAMoveAndSinceSkipsEarlierEntries(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	if got := timelineJSON(t, d, "demo"); len(got) != 0 {
		t.Fatalf("a new ticket has entries: %+v", got)
	}
	if out, _, code := rig(t, d, "timeline", "demo"); code != 0 || !strings.Contains(out, "no entries") {
		t.Fatalf("empty timeline printed %q (code %d)", out, code)
	}
	if _, stderr, code := rig(t, d, "approve", "demo", "plan_approval"); code != 0 {
		t.Fatal(stderr)
	}
	if _, stderr, code := rig(t, d, "phase", "demo", "implement"); code != 0 {
		t.Fatal(stderr)
	}
	all := timelineJSON(t, d, "demo")
	if len(all) != 2 || all[0].Kind != "gate.approved" || all[1].Kind != "phase.moved" || all[0].Seq != 1 || all[1].Seq != 2 {
		t.Fatalf("timeline = %+v", all)
	}
	if later := timelineJSON(t, d, "demo", "--since", "1"); len(later) != 1 || later[0].Seq != 2 {
		t.Errorf("--since 1 = %+v", later)
	}
	out, _, _ := rig(t, d, "timeline", "demo")
	for _, want := range []string{"SEQ", "gate.approved", "gate=plan_approval", "phase.moved", "from=plan", "to=implement"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestTimelineOfAnUnknownTicketIsNotFound(t *testing.T) {
	_, d, _ := newEnv(t)
	if _, stderr, code := rig(t, d, "timeline", "nope"); code == 0 || !strings.Contains(stderr, "not found") {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
	if _, _, code := rig(t, d, "timeline", "demo", "--since", "-1"); code != 2 {
		t.Errorf("negative --since: code %d, want 2", code)
	}
}
