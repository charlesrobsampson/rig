package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/state"
)

// passRound runs a round on the coder's current commit and has the reviewer pass it.
func (r *reviewRig) passRound(round int) {
	r.t.Helper()
	if err := r.record("svc1", "review.ready", "ready", nil); err != nil {
		r.t.Fatal(err)
	}
	if err := r.record("review", "verdict", "No blocking findings", map[string]int{"round": round}); err != nil {
		r.t.Fatal(err)
	}
	if rv := r.review(); rv.Status != domain.ReviewPassed || rv.Round != round {
		r.t.Fatalf("review = %+v, want passed in round %d", rv, round)
	}
}

func TestACommitAfterAPassBlocksLeavingImplementUntilItIsReviewed(t *testing.T) {
	r := newReviewRig(t, "{}")
	passed := r.commit("a.txt", "first\n")
	r.passRound(1)
	later := r.commit("b.txt", "after the pass\n")

	_, err := r.setPhase("demo", domain.PhaseTest, "")
	if apiCode(t, err) != api.CodeBlocked || !strings.Contains(err.Error(), "the review passed "+passed[:12]+", but the branch is now at "+later[:12]) || !strings.Contains(err.Error(), "review.ready") || !strings.Contains(err.Error(), "rig reviewer demo --start") {
		t.Fatalf("leaving implement = %v", err)
	}
	if r.state("demo").Phase != domain.PhaseImplement {
		t.Error("the ticket left implement")
	}

	if err := r.record("svc1", "review.ready", "the later commit", nil); err != nil {
		t.Fatal(err)
	}
	if rv := r.review(); rv.Round != 2 || rv.Commit != later || rv.Status != domain.ReviewReviewing {
		t.Fatalf("review = %+v, want round 2 on %s", rv, later)
	}
	r.record("review", "verdict", "No blocking findings", map[string]int{"round": 2})
	if _, err := r.setPhase("demo", domain.PhaseTest, ""); err != nil {
		t.Errorf("leaving implement after the new pass: %v", err)
	}
}

func TestACommitAfterAPassBlocksThePush(t *testing.T) {
	r := newReviewRig(t, "{}")
	passed := r.commit("a.txt", "first\n")
	r.passRound(1)
	r.mustSet("demo", domain.PhaseTest, "")
	r.mustSet("demo", domain.PhaseReview, "")
	if r.requires("demo", domain.GateReviewApproval) {
		r.mustApprove("demo", domain.GateReviewApproval, "")
	}
	r.mustSet("demo", domain.PhasePush, "")
	later := r.commit("b.txt", "after the pass\n")

	res, err := r.lc.Push(ctx, api.PushRunParams{ID: "demo"})
	if err != nil || len(res.Services) != 1 || res.Done {
		t.Fatalf("push = %+v, %v", res, err)
	}
	got := res.Services[0]
	if got.Status != domain.PushFailed || !strings.Contains(got.Error, passed[:12]) || !strings.Contains(got.Error, later[:12]) {
		t.Errorf("push result = %+v, want a refusal naming %s and %s", got, passed[:12], later[:12])
	}
}

func TestAPassWithNoRecordedCommitIsNotChecked(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "first\n")
	r.passRound(1)
	// State as an older rig wrote it: passed, with no commit.
	if err := r.states.Update(filepath.Join(r.path, "state.json"), func(s *state.State) error { s.Review.Commit = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	r.commit("b.txt", "after\n")
	if _, err := r.setPhase("demo", domain.PhaseTest, ""); err != nil {
		t.Fatalf("leaving implement: %v", err)
	}
	r.mustSet("demo", domain.PhaseReview, "")
	if r.requires("demo", domain.GateReviewApproval) {
		r.mustApprove("demo", domain.GateReviewApproval, "")
	}
	r.mustSet("demo", domain.PhasePush, "")
	res, err := r.lc.Push(ctx, api.PushRunParams{ID: "demo"})
	if err != nil || len(res.Services) != 1 || strings.Contains(res.Services[0].Error, "the review passed") {
		t.Errorf("push = %+v, %v; the pin was applied", res, err)
	}
}

func TestAPersonsPassIsBoundToTheCommitItWasGivenAt(t *testing.T) {
	r := newReviewRig(t, "{}")
	given := r.commit("a.txt", "first\n")
	if _, err := r.lc.ReviewerDecide(ctx, api.ReviewerDecideParams{Ticket: "demo", Outcome: "pass", Reason: "checked by hand"}); err != nil {
		t.Fatal(err)
	}
	if rv := r.review(); rv.Commit != given {
		t.Fatalf("a person's pass pinned %q, want %s", rv.Commit, given)
	}
	later := r.commit("b.txt", "after\n")
	if _, err := r.setPhase("demo", domain.PhaseTest, ""); err == nil || !strings.Contains(err.Error(), given[:12]) || !strings.Contains(err.Error(), later[:12]) {
		t.Errorf("leaving implement = %v", err)
	}
}

func TestASyncChangingAPassedBranchEndsTheLoop(t *testing.T) {
	t.Run("a conflict: the coder's review.ready starts a new loop at round 1", func(t *testing.T) {
		r := newReviewRig(t, `{max_rounds: 1}`)
		r.commit("shared.txt", "mine\n")
		r.passRound(1)
		r.repo.Commit("shared.txt", "theirs\n", "the same file changes on main")

		res, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"})
		if err != nil || len(res.Services[0].Conflicts) != 1 || res.ReviewStarted {
			t.Fatalf("sync = %+v, %v", res, err)
		}
		if rv := r.review(); rv.Status != domain.ReviewAwaitingReady || rv.Round != 1 || rv.Commit != "" {
			t.Fatalf("review after the sync = %+v, want awaiting_ready, round kept, no commit", rv)
		}
		if ended := only(readTimeline(t, r.path), "review.loop.ended"); len(ended) != 1 || ended[0]["rounds"] != 1.0 || ended[0]["status"] != "passed" {
			t.Errorf("review.loop.ended = %v", ended)
		}
		if !strings.Contains(r.last("svc1"), "review.ready") {
			t.Errorf("the coder was told %q", r.last("svc1"))
		}

		// The coder resolves and commits the merge.
		for _, args := range [][]string{{"checkout", "--ours", "shared.txt"}, {"add", "shared.txt"}, {"-c", "user.name=c", "-c", "user.email=c@example.com", "commit", "-q", "--no-edit"}} {
			if _, err := git.Hardened().Run(ctx, r.coder, args...); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.record("svc1", "review.ready", "merge resolved", nil); err != nil {
			t.Fatalf("review.ready after the resolve: %v (told %q)", err, r.last("svc1"))
		}
		if rv := r.review(); rv.Round != 1 || rv.Status != domain.ReviewReviewing || len(rv.Rounds) != 1 {
			t.Fatalf("review = %+v, want round 1 of a new loop (not a cap stop)", rv)
		}
	})

	t.Run("a clean merge starts a new loop's round 1", func(t *testing.T) {
		r := newReviewRig(t, "{}")
		r.commit("a.txt", "mine\n")
		r.passRound(1)
		r.repo.Commit("other.txt", "theirs\n", "an unrelated change lands on main")

		res, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"})
		if err != nil || !res.ReviewStarted {
			t.Fatalf("sync = %+v, %v", res, err)
		}
		r.lc.Wait()
		if !strings.Contains(r.last(domain.ReviewerService), "Review round 1") {
			t.Errorf("the reviewer was not briefed: %q", r.last(domain.ReviewerService))
		}
		rv := r.review()
		if rv.Round != 1 || len(rv.Rounds) != 1 || rv.Status != domain.ReviewReviewing || rv.Commit != res.Services[0].Merged {
			t.Errorf("review = %+v, want round 1 on the merge %s", rv, res.Services[0].Merged)
		}
		if ended := only(readTimeline(t, r.path), "review.loop.ended"); len(ended) != 1 {
			t.Errorf("review.loop.ended = %v, want one", ended)
		}
	})

	t.Run("from review, the move back and the sync log one end", func(t *testing.T) {
		r := newReviewRig(t, "{}")
		r.commit("a.txt", "mine\n")
		r.passRound(1)
		r.mustSet("demo", domain.PhaseTest, "")
		r.mustSet("demo", domain.PhaseReview, "")
		r.repo.Commit("other.txt", "theirs\n", "main moves on")
		if _, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"}); err != nil {
			t.Fatal(err)
		}
		if ended := only(readTimeline(t, r.path), "review.loop.ended"); len(ended) != 1 {
			t.Errorf("review.loop.ended = %v, want one", ended)
		}
	})

	t.Run("a sync while a round runs leaves the loop", func(t *testing.T) {
		r := newReviewRig(t, "{}")
		r.commit("a.txt", "mine\n")
		r.record("svc1", "review.ready", "ready", nil)
		r.repo.Commit("other.txt", "theirs\n", "main moves on")
		if _, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"}); err != nil {
			t.Fatal(err)
		}
		if rv := r.review(); rv.Status != domain.ReviewReviewing || rv.Round != 1 {
			t.Errorf("review = %+v, want the running round kept", rv)
		}
		if ended := only(readTimeline(t, r.path), "review.loop.ended"); len(ended) != 0 {
			t.Errorf("review.loop.ended = %v", ended)
		}
	})
}

// resolveMerge commits the coder's resolution of a conflicting sync.
func (r *reviewRig) resolveMerge() {
	r.t.Helper()
	for _, args := range [][]string{{"checkout", "--ours", "shared.txt"}, {"add", "shared.txt"}, {"-c", "user.name=c", "-c", "user.email=c@example.com", "commit", "-q", "--no-edit"}} {
		if _, err := git.Hardened().Run(ctx, r.coder, args...); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *reviewRig) conflictAfterPass() {
	r.t.Helper()
	r.commit("shared.txt", "mine\n")
	r.passRound(1)
	r.repo.Commit("shared.txt", "theirs\n", "the same file changes on main")
	if _, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"}); err != nil {
		r.t.Fatal(err)
	}
}

func TestAfterASyncReviewReadyStartsTheRoundOnTheMergedTip(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.conflictAfterPass()
	r.resolveMerge()
	tip, _ := git.Hardened().Run(ctx, r.coder, "rev-parse", "HEAD")
	if err := r.record("svc1", "review.ready", "merge resolved", nil); err != nil {
		t.Fatalf("review.ready after a sync: %v (told %q)", err, r.last("svc1"))
	}
	if rv := r.review(); rv.Status != domain.ReviewReviewing || rv.Round != 1 || rv.Commit != strings.TrimSpace(tip) {
		t.Fatalf("review = %+v, want round 1 on %s", rv, tip)
	}
	if strings.Contains(r.last("svc1"), "was not taken") {
		t.Errorf("the coder was told %q", r.last("svc1"))
	}
}

func TestAfterASyncReviewerStartStartsTheRoundOnTheMergedTip(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.conflictAfterPass()
	r.resolveMerge()
	tip, _ := git.Hardened().Run(ctx, r.coder, "rev-parse", "HEAD")
	if _, err := r.lc.ReviewerStart(ctx, api.ReviewerParams{Ticket: "demo"}); err != nil {
		t.Fatalf("--start after a sync: %v", err)
	}
	if rv := r.review(); rv.Status != domain.ReviewReviewing || rv.Round != 1 || rv.Commit != strings.TrimSpace(tip) {
		t.Fatalf("review = %+v, want round 1 on %s", rv, tip)
	}
}

// r121: round 3 passed, then rig sync merged cleanly; the round must pin the merged
// tip without anyone journaling review.ready.
func TestACleanSyncAfterAPassPinsTheMergedTipWithoutReviewReady(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "one\n")
	r.passRound(1)
	r.commit("b.txt", "two\n")
	r.passRound(2)
	r.repo.Commit("other.txt", "theirs\n", "an unrelated change lands on main")
	res, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"})
	if err != nil || !res.ReviewStarted {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	r.lc.Wait()
	rv := r.review()
	if rv.Status != domain.ReviewReviewing || rv.Round != 1 || rv.Commit != res.Services[0].Merged || rv.Commit == "" {
		t.Fatalf("review = %+v, want round 1 pinned on the merge %s", rv, res.Services[0].Merged)
	}
}

func TestAStuckRoundLoadsAsAwaitingReady(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "mine\n")
	if err := r.states.Update(filepath.Join(r.path, "state.json"), func(s *state.State) error {
		s.Review = &state.Review{Status: domain.ReviewReviewing, Round: 2, Rounds: []state.ReviewRound{{Round: 1}, {Round: 2}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.record("svc1", "review.ready", "ready", nil); err != nil {
		t.Fatalf("review.ready on a stuck round: %v", err)
	}
	if rv := r.review(); rv.Status != domain.ReviewReviewing || rv.Commit == "" || rv.Round != 2 {
		t.Fatalf("review = %+v, want round 2 of the same loop pinned", rv)
	}

	// The stuck state is normalised when read, even with no request that starts a round.
	if err := r.states.Update(filepath.Join(r.path, "state.json"), func(s *state.State) error {
		s.Review = &state.Review{Status: domain.ReviewReviewing, Round: 2}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.lc.ReviewerStatus(ctx, api.ReviewerParams{Ticket: "demo"}); err != nil {
		t.Fatal(err)
	}
	if rv := r.review(); rv.Status != domain.ReviewAwaitingReady || rv.Round != 2 {
		t.Fatalf("review = %+v, want awaiting_ready, round kept", rv)
	}
}

func TestACleanSyncFromTestOrReviewAfterAPassPinsTheMergedTip(t *testing.T) {
	for _, phase := range []domain.Phase{domain.PhaseTest, domain.PhaseReview} {
		t.Run(string(phase), func(t *testing.T) {
			r := newReviewRig(t, "{}")
			r.commit("a.txt", "one\n")
			r.passRound(1)
			r.mustSet("demo", domain.PhaseTest, "")
			if phase == domain.PhaseReview {
				r.mustSet("demo", domain.PhaseReview, "")
			}
			r.repo.Commit("other.txt", "theirs\n", "an unrelated change lands on main")
			res, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"})
			if err != nil || !res.ReviewStarted || res.Phase != domain.PhaseImplement {
				t.Fatalf("sync = %+v, %v", res, err)
			}
			r.lc.Wait()
			rv := r.review()
			if rv.Status != domain.ReviewReviewing || rv.Round != 1 || rv.Commit != res.Services[0].Merged || rv.Commit == "" {
				t.Fatalf("review = %+v, want round 1 pinned on the merge %s", rv, res.Services[0].Merged)
			}
		})
	}
}

func TestARoundThatIsOnlyStartingIsNotTakenForALostOne(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "mine\n")
	if err := r.lc.BeginRound("demo", "agent:svc1"); err != nil {
		t.Fatal(err)
	}
	// Reads while the round is starting leave it alone.
	if _, err := r.lc.ReviewerStatus(ctx, api.ReviewerParams{Ticket: "demo"}); err != nil {
		t.Fatal(err)
	}
	if rv := r.review(); rv.Status != domain.ReviewReviewing {
		t.Fatalf("a starting round became %s", rv.Status)
	}
	if err := r.lc.BeginRound("demo", "agent:svc1"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("a second review.ready = %v, want refused as running", err)
	}
	r.lc.RunRound(ctx, "demo")
	if rv := r.review(); rv.Status != domain.ReviewReviewing || rv.Commit == "" {
		t.Fatalf("review = %+v, want the round pinned", rv)
	}
}

func TestARoundCancelledByShutdownWaitsForReadyAndKeepsTheLoop(t *testing.T) {
	r := newReviewRig(t, `{max_rounds: 3}`)
	r.commit("a.txt", "mine\n")
	r.record("svc1", "review.ready", "ready", nil)
	r.record("review", "review.result", "x", map[string]string{"id": "F1", "severity": "blocking", "what": "w"})
	r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": 1})
	r.commit("b.txt", "fix\n")
	before := r.review()

	if err := r.lc.BeginRound("demo", "agent:svc1"); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	r.lc.RunRound(gone, "demo")
	rv := r.review()
	if rv.Status != domain.ReviewAwaitingReady || rv.Commit != "" || rv.Round != 2 {
		t.Fatalf("review = %+v, want awaiting_ready (not stopped), round 2 kept", rv)
	}

	// review.ready counts the lost round again instead of starting a fresh loop.
	if err := r.record("svc1", "review.ready", "ready again", nil); err != nil {
		t.Fatal(err)
	}
	rv = r.review()
	if rv.Status != domain.ReviewReviewing || rv.Round != 2 || len(rv.Rounds) != 2 || rv.Commit == "" || rv.CostAtStart != before.CostAtStart {
		t.Fatalf("review = %+v, want round 2 of the same loop (cost at start %v)", rv, before.CostAtStart)
	}
}

func TestACleanSyncWhoseRoundCannotStartTellsTheCoderAndWaits(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "mine\n")
	r.passRound(1)
	r.repo.Commit("other.txt", "theirs\n", "main moves on")
	// The coder reacts to the merge message by leaving work uncommitted, so the round
	// cannot start (the reviewer sees commits only).
	r.onDeliver = func(service, text string) {
		if service == "svc1" && strings.Contains(text, "rig merged main") {
			os.WriteFile(filepath.Join(r.coder, "wip.txt"), []byte("x\n"), 0o644)
		}
	}
	res, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"})
	if err != nil || res.ReviewStarted {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	r.lc.Wait()
	if rv := r.review(); rv.Status != domain.ReviewAwaitingReady || rv.Commit != "" || rv.Round != 1 {
		t.Fatalf("review = %+v, want awaiting_ready, round kept", rv)
	}
	if msg := r.last("svc1"); !strings.Contains(msg, "did not start") || !strings.Contains(msg, "uncommitted work") || !strings.Contains(msg, "review.ready") {
		t.Errorf("the coder was told %q", msg)
	}
	// Committing and journaling review.ready then starts the round.
	r.onDeliver = nil
	r.commit("wip.txt", "x\n")
	if err := r.record("svc1", "review.ready", "committed", nil); err != nil {
		t.Fatal(err)
	}
	if rv := r.review(); rv.Status != domain.ReviewReviewing || rv.Commit == "" {
		t.Fatalf("review = %+v", rv)
	}
	if rej := only(readTimeline(t, r.path), "record.rejected"); len(rej) != 0 {
		t.Errorf("rejected records: %v", rej)
	}
}
