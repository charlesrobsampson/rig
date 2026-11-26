package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// passedTicketIn is a reviewed ticket whose review passed and that a person moved on to
// the given phase.
func passedTicketIn(t *testing.T, to domain.Phase) *reviewRig {
	t.Helper()
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "mine\n")
	if _, err := r.lc.ReviewerDecide(ctx, api.ReviewerDecideParams{Ticket: "demo", Outcome: "pass", Reason: "checked by hand"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []domain.Phase{domain.PhaseTest, domain.PhaseReview} {
		r.mustSet("demo", p, "")
		if p == to {
			break
		}
	}
	return r
}

func (r *reviewRig) syncAfterMainMoves() {
	r.t.Helper()
	r.repo.Commit("other.txt", "theirs\n", "main moves on")
	if res, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"}); err != nil || res.Phase != domain.PhaseImplement || !res.ReviewStarted {
		r.t.Fatalf("sync = %+v, %v", res, err)
	}
}

func (r *reviewRig) verdictPass(round int) {
	r.t.Helper()
	r.record("review", "verdict", "No blocking findings", map[string]int{"round": round})
}

func TestAfterASyncAPassingRoundReturnsTheTicketToItsPhase(t *testing.T) {
	for _, from := range []domain.Phase{domain.PhaseTest, domain.PhaseReview} {
		t.Run(string(from), func(t *testing.T) {
			r := passedTicketIn(t, from)
			r.syncAfterMainMoves()
			if got := r.state("demo").SyncReturn; got != from {
				t.Fatalf("sync_return = %q, want %q", got, from)
			}
			r.verdictPass(1)
			st := r.state("demo")
			if st.Phase != from || st.SyncReturn != "" {
				t.Fatalf("phase %s, sync_return %q; want %s and none", st.Phase, st.SyncReturn, from)
			}
			var steps []string
			for _, tr := range st.Transitions[len(st.Transitions)-2:] {
				steps = append(steps, string(tr.From)+">"+string(tr.To)+" by "+tr.By)
			}
			want := []string{"implement>test by rig"}
			if from == domain.PhaseReview {
				want = []string{"implement>test by rig", "test>review by rig"}
			}
			if !strings.HasSuffix(strings.Join(steps, ","), strings.Join(want, ",")) {
				t.Errorf("transitions = %v, want %v", steps, want)
			}
			moved := only(readTimeline(t, r.path), "phase.moved")
			last := moved[len(moved)-1]
			if last["reason"] != "review passed on the merged work" || last["to"] != string(from) {
				t.Errorf("last phase.moved = %v", last)
			}
			if len(st.Approvals) != 0 {
				t.Errorf("approvals came back: %+v", st.Approvals)
			}
			if from == domain.PhaseReview && r.requires("demo", domain.GateReviewApproval) {
				if _, err := r.setPhase("demo", domain.PhasePush, ""); apiCode(t, err) != api.CodeBlocked {
					t.Errorf("the gate no longer needs a person: %v", err)
				}
			}
		})
	}
}

func TestAPersonsMoveBetweenSyncAndPassCancelsTheReturn(t *testing.T) {
	r := passedTicketIn(t, domain.PhaseTest)
	r.syncAfterMainMoves()
	r.mustSet("demo", domain.PhasePlan, "rethink")
	r.mustSet("demo", domain.PhaseImplement, "")
	if got := r.state("demo").SyncReturn; got != "" {
		t.Fatalf("sync_return = %q", got)
	}
	r.commit("b.txt", "x\n")
	r.record("svc1", "review.ready", "", nil)
	r.verdictPass(r.review().Round)
	if st := r.state("demo"); st.Phase != domain.PhaseImplement {
		t.Errorf("phase = %s, want implement", st.Phase)
	}
}

func TestASyncFromImplementOrWithoutAReviewRecordsNoReturn(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "mine\n")
	r.repo.Commit("other.txt", "theirs\n", "main moves on")
	if _, err := r.lc.Sync(ctx, api.SyncParams{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	r.verdictPass(1)
	if st := r.state("demo"); st.SyncReturn != "" || st.Phase != domain.PhaseImplement {
		t.Errorf("phase %s, sync_return %q", st.Phase, st.SyncReturn)
	}

	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	f.create("plain", repo)
	f.mustStart("plain", domain.SandboxContainer)
	f.advanceTo("plain", domain.PhaseTest)
	repo.Commit("other.txt", "theirs\n", "main moves on")
	if res, err := f.lc.Sync(ctx, api.SyncParams{ID: "plain"}); err != nil || res.Phase != domain.PhaseImplement {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	if got := f.state("plain").SyncReturn; got != "" {
		t.Errorf("sync_return = %q for a ticket with no review", got)
	}
}

// roundWithFinding runs a round that ends with one blocking finding F1.
func (r *reviewRig) roundWithFinding(file string) {
	r.t.Helper()
	r.commit(file, "x\n")
	r.record("svc1", "review.ready", "", nil)
	r.record("review", "review.result", "", finding("F1", "blocking", "crashes on empty input"))
	r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": r.review().Round})
}

func TestAQuestionStopsTheLoopWithoutUsingARound(t *testing.T) {
	r := newReviewRig(t, "{max_rounds: 3}")
	r.roundWithFinding("a.txt")
	r.roundWithFinding("b.txt")
	r.roundWithFinding("c.txt") // round 3 of 3
	if rv := r.review(); rv.Round != 3 || rv.Status != domain.ReviewFixing {
		t.Fatalf("review = %+v", rv)
	}
	if err := r.record("svc1", "review.reply", "", map[string]string{"id": "F1", "status": "question", "reason": "should empty input be an error or a no-op?"}); err != nil {
		t.Fatal(err)
	}
	rv := r.review()
	if rv.Status != domain.ReviewStopped || rv.By != "rig" || rv.Round != 3 || len(rv.Rounds) != 3 ||
		!strings.Contains(rv.Reason, "F1") || !strings.Contains(rv.Reason, "should empty input be an error or a no-op?") {
		t.Fatalf("review = %+v, want stopped on the question at round 3", rv)
	}
	if msg := r.last("svc1"); !strings.Contains(msg, "Wait for a person") {
		t.Errorf("coder told %q", msg)
	}
	if err := r.record("svc1", "review.ready", "", nil); err == nil {
		t.Error("an agent restarted a loop stopped for its own question")
	}

	// The person answers and resumes: round 4 runs although the cap is 3, and can pass.
	res, err := r.lc.ReviewerStart(ctx, api.ReviewerParams{Ticket: "demo"})
	if err != nil || res.Status != domain.ReviewReviewing || res.Round != 4 {
		t.Fatalf("resume = %+v, %v", res, err)
	}
	if res.Commit == "" || len(res.Rounds) != 4 {
		t.Errorf("the loop did not continue: %+v", res)
	}
	r.verdictPass(4)
	if rv := r.review(); rv.Status != domain.ReviewPassed {
		t.Fatalf("review = %+v, want passed", rv)
	}
}

func TestTheCapStillHoldsAfterAQuestion(t *testing.T) {
	r := newReviewRig(t, "{max_rounds: 1}")
	r.roundWithFinding("a.txt")
	r.record("svc1", "review.reply", "", map[string]string{"id": "F1", "status": "question", "reason": "which?"})
	if _, err := r.lc.ReviewerStart(ctx, api.ReviewerParams{Ticket: "demo"}); err != nil {
		t.Fatal(err)
	}
	r.record("review", "review.result", "", finding("F1", "blocking", "still crashes"))
	r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": 2})
	r.commit("b.txt", "x\n")
	r.record("svc1", "review.ready", "", nil)
	if rv := r.review(); rv.Status != domain.ReviewStopped || !strings.Contains(rv.Reason, "round cap (1)") || rv.Round != 2 {
		t.Fatalf("review = %+v, want the cap after one counted round", rv)
	}
}

func TestOtherRepliesAndStopsStartANewLoopAsBefore(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.roundWithFinding("a.txt")
	r.record("svc1", "review.reply", "", map[string]string{"id": "F1", "status": "fixed"})
	if rv := r.review(); rv.Status != domain.ReviewFixing {
		t.Fatalf("a fixed reply changed the loop: %+v", rv)
	}
	if _, err := r.lc.ReviewerDecide(ctx, api.ReviewerDecideParams{Ticket: "demo", Outcome: "stop", Reason: "enough"}); err != nil {
		t.Fatal(err)
	}
	if res, err := r.lc.ReviewerStart(ctx, api.ReviewerParams{Ticket: "demo"}); err != nil || res.Round != 1 {
		t.Errorf("restart after a person's stop = %+v, %v; want a new loop at round 1", res, err)
	}
}
