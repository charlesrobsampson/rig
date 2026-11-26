package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
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
		if st := r.state("demo"); st.Review != nil {
			t.Fatalf("review after the sync = %+v, want the loop ended", st.Review)
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
