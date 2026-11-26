package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

func (r *reviewRig) spawnPrompt(service string) string {
	for _, s := range r.term.Spawned() {
		if s.Service == service {
			return s.Command[len(s.Command)-1]
		}
	}
	return ""
}

func tested(findings ...map[string]string) map[string]any {
	out := map[string]any{"outcome": "pass"}
	if len(findings) > 0 {
		out["findings"] = findings
	}
	return out
}

func testFail(findings ...map[string]string) map[string]any {
	out := tested(findings...)
	out["outcome"] = "fail"
	return out
}

func tfinding(severity, text string) map[string]string {
	return map[string]string{"severity": severity, "text": text}
}

func verdictFor(r *reviewRig, round int, text string) {
	r.t.Helper()
	if err := r.record("review", "verdict", text, map[string]int{"round": round}); err != nil {
		r.t.Fatal(err)
	}
}

func TestARoundWithATesterWaitsForBothAndPassesWhenBothPass(t *testing.T) {
	for _, order := range []string{"reviewer first", "tester first"} {
		t.Run(order, func(t *testing.T) {
			r := newReviewRig(t, "{tester: {}}")
			r.commit("a.txt", "first\n")
			r.record("svc1", "review.ready", "done", nil)
			if r.review().Status != domain.ReviewReviewing {
				t.Fatalf("review = %+v", r.review())
			}
			if order == "reviewer first" {
				verdictFor(r, 1, "No blocking findings")
			} else if err := r.record("tester", "test.result", "all steps pass", tested()); err != nil {
				t.Fatal(err)
			}
			if rv := r.review(); rv.Status != domain.ReviewReviewing {
				t.Fatalf("settled with one checker still out: %+v", rv)
			}
			if order == "reviewer first" {
				if err := r.record("tester", "test.result", "all steps pass", tested()); err != nil {
					t.Fatal(err)
				}
			} else {
				verdictFor(r, 1, "No blocking findings")
			}
			if rv := r.review(); rv.Status != domain.ReviewPassed || rv.Rounds[0].Tester == nil || rv.Rounds[0].Tester.Outcome != "pass" {
				t.Fatalf("review = %+v, want passed with the tester's outcome", rv)
			}
			if _, err := r.setPhase("demo", domain.PhaseTest, ""); err != nil {
				t.Errorf("the move after a joint pass: %v", err)
			}
		})
	}
}

func TestATesterFailSendsItsFindingsToTheCoderLabelledAndDoesNotPass(t *testing.T) {
	r := newReviewRig(t, "{tester: {}}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "done", nil)
	verdictFor(r, 1, "No blocking findings")
	r.record("tester", "test.result", "the CLI crashes", testFail(tfinding("blocking", "run it with no args: stack trace, expected usage"), tfinding("nit", "help text typo")))
	if rv := r.review(); rv.Status != domain.ReviewFixing {
		t.Fatalf("review = %+v, want fixing", rv)
	}
	msg := r.last("svc1")
	if !strings.Contains(msg, "T1 [blocking] (tester): run it with no args") || !strings.Contains(msg, "Nits") || !strings.Contains(msg, "T2 [nit] (tester)") {
		t.Errorf("coder got %q", msg)
	}
	if _, err := r.setPhase("demo", domain.PhaseTest, ""); err == nil {
		t.Error("left implement with a failed test")
	}
	status, _ := r.lc.ReviewerStatus(ctx, api.ReviewerParams{Ticket: "demo"})
	if tr := status.Rounds[0].Tester; tr == nil || tr.Outcome != "fail" || len(tr.Findings) != 2 || tr.Findings[0].Source != "tester" {
		t.Errorf("status tester = %+v", tr)
	}

	// Reviewer findings and the tester's arrive together, each labelled.
	fixed := r.commit("a.txt", "fixed\n")
	r.record("svc1", "review.reply", "", map[string]string{"id": "T1", "status": "fixed", "commit": fixed})
	r.record("svc1", "review.ready", "fixed", nil)
	r.record("review", "review.result", "", finding("F1", "should-fix", "no test"))
	verdictFor(r, 2, "Blocking findings: 0")
	r.record("tester", "test.result", "fine now", tested())
	if msg := r.last("svc1"); !strings.Contains(msg, "found 1 to fix") || strings.Contains(msg, "(tester)") || !strings.Contains(msg, "F1 [should-fix]") {
		t.Errorf("round 2 message = %q", msg)
	}
}

func TestATesterFailWithoutFindingsStillBlocks(t *testing.T) {
	r := newReviewRig(t, "{tester: {}}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	verdictFor(r, 1, "No blocking findings")
	r.record("tester", "test.result", "", testFail(tfinding("nit", "color")))
	if rv := r.review(); rv.Status != domain.ReviewFixing || !strings.Contains(r.last("svc1"), "T0 [blocking] (tester)") {
		t.Errorf("review = %+v, coder got %q", rv, r.last("svc1"))
	}
}

func TestOnlyTheTesterGivesATestResultOnceAndOnlyWithATester(t *testing.T) {
	r := newReviewRig(t, "{tester: {}}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	for name, err := range map[string]error{
		"coder":    r.record("svc1", "test.result", "", tested()),
		"reviewer": r.record("review", "test.result", "", tested()),
		"bad data": r.record("tester", "test.result", "", map[string]string{"outcome": "maybe"}),
		"severity": r.record("tester", "test.result", "", testFail(tfinding("huge", "x"))),
	} {
		if err == nil {
			t.Errorf("%s: a test.result was taken", name)
		}
	}
	if err := r.record("tester", "verdict", "x", map[string]int{"round": 1}); err == nil {
		t.Error("the tester gave a verdict")
	}
	if err := r.record("tester", "test.result", "", tested()); err != nil {
		t.Fatal(err)
	}
	if err := r.record("tester", "test.result", "", tested()); err == nil {
		t.Error("a second test.result in one round was taken")
	}

	plain := newReviewRig(t, "{}")
	plain.commit("a.txt", "first\n")
	plain.record("svc1", "review.ready", "", nil)
	if err := plain.record("tester", "test.result", "", tested()); err == nil {
		t.Error("a ticket without a tester took a test.result")
	}
}

func TestTheTesterGetsItsOwnCopyOfThePinnedCommitInATmuxImage(t *testing.T) {
	brief := filepath.Join(t.TempDir(), "brief.md")
	os.WriteFile(brief, []byte("Try the CLI like a new user."), 0o644)
	r := newReviewRig(t, "{copy: [node_modules], tester: {brief: "+brief+", model: sonnet}}")
	os.MkdirAll(filepath.Join(r.coder, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(r.coder, "node_modules", "dep.js"), []byte("1"), 0o644)
	sha := r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "done", nil)
	// The coder moves on while the round runs; the tester must not see it.
	r.commit("b.txt", "later\n")

	dir := filepath.Join(r.path, "tester")
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "first\n" {
		t.Errorf("the tester's a.txt = %q, want the pinned commit", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Error("the tester sees a later commit")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "node_modules", "dep.js")); string(got) != "1" {
		t.Error("review.copy was not applied to the tester's copy")
	}
	if dir == filepath.Join(r.path, "review") {
		t.Fatal("the tester shares the reviewer's copy")
	}
	c, err := r.dk.InspectContainer(ctx, "rig-demo-tester-agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mountSet(c) {
		if strings.Contains(m, r.coder) || strings.Contains(m, filepath.Join(r.path, "review")) {
			t.Errorf("the tester has the coder's or reviewer's tree mounted: %s", m)
		}
		if strings.Contains(m, "/rig/ticket/") && !strings.HasSuffix(m, " ro") {
			t.Errorf("a ticket file is writable by the tester: %s", m)
		}
	}
	tmux := false
	for _, b := range r.dk.Builds() {
		tmux = tmux || strings.HasPrefix(b.Tag, "rig-agent-tmux:")
	}
	if !tmux || !strings.HasPrefix(r.dk.ContainerSpec("rig-demo-tester-agent").Image, "rig-agent-tmux:") {
		t.Errorf("the tester did not run in the tmux image: %+v", r.dk.ContainerSpec("rig-demo-tester-agent"))
	}
	if rc := r.dk.ContainerSpec("rig-demo-review-agent"); strings.HasPrefix(rc.Image, "rig-agent-tmux:") {
		t.Error("the reviewer runs in the tmux image")
	}

	prompt := r.spawnPrompt("tester")
	if !strings.HasPrefix(prompt, "Try the CLI like a new user.") || !strings.Contains(prompt, `tester of service "svc1"`) || !strings.Contains(prompt, "test.result") {
		t.Errorf("tester prompt = %.300q", prompt)
	}
	if got := r.spawnPrompt("review"); strings.Contains(got, "Try the CLI") {
		t.Error("the reviewer got the tester's brief")
	}
	b := r.last("tester")
	if !strings.Contains(b, "Test round 1") || !strings.Contains(b, sha) || !strings.Contains(b, "/work") || !strings.Contains(b, "plan.md") {
		t.Errorf("round brief = %q", b)
	}
	if !strings.Contains(r.last("svc1"), "Review round 1 started") {
		t.Errorf("coder told %q", r.last("svc1"))
	}

	// Round 2 is typed to the running tester and its copy is made again.
	os.WriteFile(filepath.Join(dir, "planted.txt"), []byte("x"), 0o644)
	verdictFor(r, 1, "No blocking findings")
	r.record("tester", "test.result", "", tested())
	r.record("svc1", "review.ready", "again", nil)
	if _, err := os.Stat(filepath.Join(dir, "planted.txt")); err == nil {
		t.Error("the tester's planted file survived into round 2")
	}
	n := 0
	for _, s := range r.term.Spawned() {
		if s.Service == "tester" {
			n++
		}
	}
	if n != 1 || !strings.Contains(r.last("tester"), "Test round 2") {
		t.Errorf("tester launched %d times; round 2 brief %q", n, r.last("tester"))
	}
}

func TestTheDefaultBriefIsUsedWithoutABriefFile(t *testing.T) {
	r := newReviewRig(t, "{tester: {}}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	if p := r.spawnPrompt("tester"); !strings.Contains(p, "Use the built product as a user would") || !strings.Contains(p, "Never edit code") || !strings.Contains(p, "cannot be checked here") {
		t.Errorf("default brief missing: %.300q", p)
	}
}

func TestTheTestersCostCountsInTheBudgetAndStats(t *testing.T) {
	r := newReviewRig(t, "{budget_usd: 2, tester: {}}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	verdictFor(r, 1, "Blocking findings: 0")
	r.record("tester", "test.result", "", testFail(tfinding("blocking", "broken")))
	r.states.Update(filepath.Join(r.path, "state.json"), func(s *state.State) error {
		ss := s.Services["tester"]
		ss.Agent.Cost = &state.Cost{USD: 2.5}
		s.Services["tester"] = ss
		return nil
	})
	status, _ := r.lc.ReviewerStatus(ctx, api.ReviewerParams{Ticket: "demo"})
	if status.TesterUSD != 2.5 || status.SpentUSD != 2.5 {
		t.Errorf("status = tester $%.2f, spent $%.2f", status.TesterUSD, status.SpentUSD)
	}
	r.commit("b.txt", "more\n")
	r.record("svc1", "review.ready", "", nil)
	if rv := r.review(); rv.Status != domain.ReviewStopped || !strings.Contains(rv.Reason, "budget of $2.00") {
		t.Errorf("review = %+v, want stopped for the budget the tester's spend used", rv)
	}
	stats, err := r.lc.Stats(ctx, api.StatsParams{IDs: []string{"demo"}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range stats.Tickets[0].Agents {
		if a.Service == "tester" {
			found = a.Role == "tester" && a.CostUSD == 2.5
		}
	}
	if !found || stats.Tickets[0].CostUSD != 2.5 {
		t.Errorf("stats = %+v", stats.Tickets[0])
	}
}

func TestADisagreeOnATestersBlockingFindingStopsTheLoop(t *testing.T) {
	r := newReviewRig(t, "{tester: {}}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	r.record("review", "review.result", "", finding("F1", "nit", "taste"))
	verdictFor(r, 1, "No blocking findings")
	r.record("tester", "test.result", "", testFail(tfinding("blocking", "crash on empty input")))
	r.record("svc1", "review.reply", "", map[string]string{"id": "T1", "status": "disagree", "reason": "cannot happen"})
	r.commit("b.txt", "more\n")
	r.record("svc1", "review.ready", "", nil)
	if rv := r.review(); rv.Status != domain.ReviewStopped || !strings.Contains(rv.Reason, "disagrees with blocking finding T1") {
		t.Errorf("review = %+v, want stopped for a person", rv)
	}
}

func TestATicketWithoutATesterIsUnchanged(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	if _, err := os.Stat(filepath.Join(r.path, "tester")); err == nil {
		t.Error("a tester's copy was made")
	}
	for _, s := range r.term.Spawned() {
		if s.Service == "tester" {
			t.Error("a tester was launched")
		}
	}
	verdictFor(r, 1, "No blocking findings")
	if rv := r.review(); rv.Status != domain.ReviewPassed || rv.Rounds[0].Tester != nil {
		t.Errorf("review = %+v, want passed on the reviewer alone", rv)
	}
	status, _ := r.lc.ReviewerStatus(ctx, api.ReviewerParams{Ticket: "demo"})
	if status.Rounds[0].Tester != nil || status.TesterUSD != 0 {
		t.Errorf("status shows a tester: %+v", status)
	}
}

func TestASyncRoundStartsTheTesterToo(t *testing.T) {
	r := newReviewRig(t, "{tester: {}}")
	r.commit("a.txt", "mine\n")
	r.repo.Commit("other.txt", "theirs\n", "lands on main")
	res, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"})
	if err != nil || !res.ReviewStarted {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	r.lc.Wait()
	if got, _ := os.ReadFile(filepath.Join(r.path, "tester", "other.txt")); string(got) != "theirs\n" {
		t.Errorf("the tester's copy lacks the merged work: %q", got)
	}
	if !strings.Contains(r.last("tester"), "Test round 1") {
		t.Errorf("tester told %q", r.last("tester"))
	}
}

func TestTestResultIsOnTheTimelineAndStoppingRemovesTheTester(t *testing.T) {
	r := newReviewRig(t, "{tester: {}}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	r.record("tester", "test.result", "", testFail(tfinding("blocking", "a"), tfinding("should-fix", "b"), tfinding("nit", "c")))
	got := only(readTimeline(t, r.path), "test.result")
	if len(got) != 1 || got[0]["outcome"] != "fail" || got[0]["blocking"] != 1.0 || got[0]["should_fix"] != 1.0 || got[0]["nit"] != 1.0 || got[0]["round"] != 1.0 {
		t.Errorf("test.result = %v", got)
	}
	if _, err := r.lc.Stop(ctx, api.TicketStopParams{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dk.InspectContainer(ctx, "rig-demo-tester-agent"); err == nil {
		t.Error("the tester's container is still there")
	}
	if _, err := os.Stat(filepath.Join(r.path, "tester")); !os.IsNotExist(err) {
		t.Error("the tester's copy is still there")
	}
	if st := r.state("demo"); st.Services["tester"].Agent.PaneID != "" {
		t.Errorf("the tester's pane is still recorded: %+v", st.Services["tester"].Agent)
	}
}
