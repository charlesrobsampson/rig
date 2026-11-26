package lifecycle_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
	tline "github.com/charlesrobsampson/rig/internal/timeline"
)

func readTimeline(t *testing.T, dir string) []tline.Entry {
	t.Helper()
	got, err := tline.Read(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range got {
		if e.Seq != i+1 {
			t.Fatalf("seq %d at position %d: %+v", e.Seq, i, got)
		}
	}
	return got
}

func kindsOf(entries []tline.Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Kind)
	}
	return out
}

// only returns the entries of one kind, each decoded into a map.
func only(entries []tline.Entry, kind string) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		if e.Kind == kind {
			var d map[string]any
			json.Unmarshal(e.Data, &d)
			out = append(out, d)
		}
	}
	return out
}

func TestTheLifecycleActionsEachAppendOneEntryAndRefusedOnesNone(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxHost, []domain.Gate{domain.GatePlanApproval})
	path := f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxHost)
	if got := kindsOf(readTimeline(t, path)); len(got) != 1 || got[0] != tline.TicketStarted {
		t.Fatalf("after start: %v", got)
	}
	f.mustStart("demo", domain.SandboxHost) // nothing changed: nothing recorded
	if n := len(readTimeline(t, path)); n != 1 {
		t.Fatalf("a start that changed nothing appended: %d entries", n)
	}

	if _, err := f.setPhase("demo", domain.PhaseImplement, ""); err == nil {
		t.Fatal("moved without the gate")
	}
	if n := len(readTimeline(t, path)); n != 1 {
		t.Fatalf("a refused move appended: %d entries", n)
	}
	f.mustApprove("demo", domain.GatePlanApproval, "")
	f.mustApprove("demo", domain.GatePlanApproval, "") // already given
	f.mustSet("demo", domain.PhaseImplement, "")
	if err := f.lc.AskForPhase("demo", "svc1", domain.PhaseTest, "done"); err != nil {
		t.Fatal(err)
	}
	if err := f.lc.AskForPhase("demo", "svc1", domain.PhaseReview, "skips test"); err == nil {
		t.Fatal("a skipped phase was recorded")
	}
	f.mustSet("demo", domain.PhaseTest, "")
	f.mustSet("demo", domain.PhaseImplement, "tests failed")
	f.mustStop("demo", true, false)

	got := readTimeline(t, path)
	want := []string{"ticket.started", "gate.approved", "phase.moved", "phase.requested", "phase.moved", "phase.moved", "ticket.stopped"}
	if strings.Join(kindsOf(got), " ") != strings.Join(want, " ") {
		t.Fatalf("kinds = %v\nwant    %v", kindsOf(got), want)
	}
	if d := only(got, "gate.approved")[0]; d["gate"] != "plan_approval" {
		t.Errorf("gate.approved data = %v", d)
	}
	if d := only(got, "phase.requested")[0]; d["to"] != "test" || d["reason"] != "done" || got[3].By != "agent:svc1" || got[3].Service != "svc1" {
		t.Errorf("phase.requested = %v, %+v", d, got[3])
	}
	if d := only(got, "phase.moved")[2]; d["from"] != "test" || d["to"] != "implement" || d["reason"] != "tests failed" || got[5].By != "human" {
		t.Errorf("phase.moved = %v, by %q", d, got[5].By)
	}
}

func TestAReviewRoundsAreRecordedAndSurviveAResetLoop(t *testing.T) {
	r := newReviewRig(t, "{}")
	sha1 := r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "done", nil)
	r.record("review", "review.result", "x", finding("F1", "blocking", "crashes"))
	r.record("review", "review.result", "x", finding("F2", "should-fix", "brittle"))
	r.record("review", "review.result", "x", finding("F3", "nit", "name"))
	r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": 1})
	sha2 := r.commit("a.txt", "fixed\n")
	r.record("svc1", "review.ready", "fixed", nil)
	r.record("review", "verdict", "No blocking findings", map[string]int{"round": 2})

	got := readTimeline(t, r.path)
	started := only(got, "review.round.started")
	if len(started) != 2 || started[0]["round"] != 1.0 || started[0]["commit"] != sha1 || started[1]["round"] != 2.0 || started[1]["commit"] != sha2 {
		t.Fatalf("review.round.started = %v", started)
	}
	closed := only(got, "review.round.closed")
	if len(closed) != 2 || closed[0]["blocking"] != 1.0 || closed[0]["should_fix"] != 1.0 || closed[0]["nit"] != 1.0 || closed[0]["verdict"] != "Blocking findings: 1" || closed[1]["blocking"] != 0.0 {
		t.Fatalf("review.round.closed = %v", closed)
	}
	if d := only(got, "review.decided"); len(d) != 1 || d[0]["outcome"] != "pass" {
		t.Fatalf("review.decided = %v", d)
	}
	if n := len(only(got, "review.loop.ended")); n != 0 {
		t.Fatalf("a loop that passed was reported as reset")
	}

	// Sent back to implement: the loop in state.json is dropped, the timeline keeps it.
	r.mustSet("demo", domain.PhaseTest, "")
	r.mustSet("demo", domain.PhaseImplement, "found a bug in test")
	got = readTimeline(t, r.path)
	if ended := only(got, "review.loop.ended"); len(ended) != 1 || ended[0]["rounds"] != 2.0 || ended[0]["status"] != "passed" {
		t.Fatalf("review.loop.ended = %v", ended)
	}
	r.commit("b.txt", "more\n")
	r.record("svc1", "review.ready", "again", nil)
	if rv := r.review(); rv.Round != 1 || len(rv.Rounds) != 1 {
		t.Fatalf("the new loop did not start fresh: %+v", rv)
	}
	if n := len(only(readTimeline(t, r.path), "review.round.started")); n != 3 {
		t.Fatalf("round starts in the timeline = %d, want 3 across both loops", n)
	}
	stats, err := r.lc.Stats(ctx, api.StatsParams{IDs: []string{"demo"}})
	if err != nil || len(stats.Tickets) != 1 || stats.Tickets[0].ReviewRounds != 3 {
		t.Fatalf("stats = %+v, %v; want 3 rounds over both loops", stats, err)
	}
}

func TestStatsFallBackToStateForATicketWithoutATimeline(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "done", nil)
	if err := os.Remove(filepath.Join(r.path, tline.FileName)); err != nil {
		t.Fatal(err)
	}
	stats, err := r.lc.Stats(ctx, api.StatsParams{IDs: []string{"demo"}})
	if err != nil || stats.Tickets[0].ReviewRounds != 1 {
		t.Fatalf("stats = %+v, %v; want the 1 round state knows", stats, err)
	}
}

func TestAPersonsReviewDecisionAndARigStopAreRecorded(t *testing.T) {
	r := newReviewRig(t, "{max_rounds: 1}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "done", nil)
	r.record("review", "review.result", "x", finding("F1", "blocking", "crashes"))
	r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": 1})
	r.commit("a.txt", "second\n")
	r.record("svc1", "review.ready", "again", nil) // over the cap: rig stops the loop

	if _, err := r.lc.ReviewerDecide(ctx, api.ReviewerDecideParams{Ticket: "demo", Outcome: "pass", Reason: "fine by me"}); err != nil {
		t.Fatal(err)
	}
	got := readTimeline(t, r.path)
	decided := only(got, "review.decided")
	if len(decided) != 2 || decided[0]["outcome"] != "stop" || !strings.Contains(decided[0]["reason"].(string), "round cap") || decided[1]["outcome"] != "pass" || decided[1]["reason"] != "fine by me" {
		t.Fatalf("review.decided = %v", decided)
	}
	for _, e := range got {
		if e.Kind == "review.decided" && e.By != map[string]string{"stop": "rig", "pass": "human"}[only([]tline.Entry{e}, e.Kind)[0]["outcome"].(string)] {
			t.Errorf("review.decided by %q: %s", e.By, e.Data)
		}
	}
}

func TestAPersonStartingANewLoopOverAStoppedOneKeepsTheOldRounds(t *testing.T) {
	r := newReviewRig(t, "{max_rounds: 1}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	r.record("review", "review.result", "", finding("F1", "blocking", "crashes"))
	r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": 1})
	r.commit("b.txt", "more\n")
	r.record("svc1", "review.ready", "", nil) // over the cap: rig stops loop 1
	if n := len(only(readTimeline(t, r.path), "review.loop.ended")); n != 0 {
		t.Fatalf("a stopped loop was reported as ended before anyone replaced it")
	}

	if res, err := r.lc.ReviewerStart(ctx, api.ReviewerParams{Ticket: "demo"}); err != nil || res.Round != 1 {
		t.Fatalf("restart = %+v, %v", res, err)
	}
	got := readTimeline(t, r.path)
	ended := only(got, "review.loop.ended")
	if len(ended) != 1 || ended[0]["rounds"] != 1.0 || ended[0]["status"] != "stopped" {
		t.Fatalf("review.loop.ended = %v", ended)
	}
	for _, e := range got {
		if e.Kind == "review.loop.ended" && e.By != "human" {
			t.Errorf("review.loop.ended by %q, want human", e.By)
		}
	}
	if n := len(only(got, "review.round.started")); n != 2 {
		t.Fatalf("round starts = %d, want 1 per loop", n)
	}
	stats, err := r.lc.Stats(ctx, api.StatsParams{IDs: []string{"demo"}})
	if err != nil || stats.Tickets[0].ReviewRounds != 2 {
		t.Fatalf("stats = %+v, %v; want both loops' rounds", stats, err)
	}
}
