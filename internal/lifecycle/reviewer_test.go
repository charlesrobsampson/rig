package lifecycle_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/journal"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

type delivered struct{ service, text string }

// reviewRig is a running one-service ticket with a review, its coder's clone, and what
// rig typed into its agents.
type reviewRig struct {
	*fixture
	dk      *testkit.FakeDocker
	term    *testkit.FakeTerminal
	repo    *testkit.Repo
	path    string // the ticket directory
	coder   string // the coder's clone
	mu      sync.Mutex
	got     []delivered
	reviewY string
}

func newReviewRig(t *testing.T, review string) *reviewRig {
	t.Helper()
	r := &reviewRig{repo: testkit.NewRepo(t)}
	r.repo.Commit(".gitignore", "node_modules/\n", "ignore installs")
	r.fixture, r.dk, r.term = agentsFixture(t, func(o *lifecycle.Options) {
		o.Deliver = func(ticket, service, text string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.got = append(r.got, delivered{service, text})
		}
	})
	r.path = r.create("demo", r.repo)
	raw, _ := os.ReadFile(filepath.Join(r.path, "ticket.yaml"))
	os.WriteFile(filepath.Join(r.path, "ticket.yaml"), append(raw, []byte("review: "+review+"\n")...), 0o600)
	res := r.mustStart("demo", domain.SandboxContainer)
	r.coder = res.Checkouts[0].Path
	r.advanceTo("demo", domain.PhaseImplement)
	return r
}

func (r *reviewRig) commit(name, content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.coder, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"-c", "user.name=c", "-c", "user.email=c@example.com", "commit", "-q", "-m", "add " + name}} {
		if _, err := git.Hardened().Run(ctx, r.coder, args...); err != nil {
			r.t.Fatal(err)
		}
	}
	sha, _ := git.Hardened().Run(ctx, r.coder, "rev-parse", "HEAD")
	return sha
}

// record is an agent's journal record as the supervisor hands it on.
func (r *reviewRig) record(service, kind, text string, data any) error {
	r.t.Helper()
	raw, _ := json.Marshal(data)
	if data == nil {
		raw = nil
	}
	if err := journal.Append(r.path, api.JournalRecord{At: time.Now().UTC(), Ticket: "demo", Service: service, Source: api.JournalSourceContainer, Kind: kind, Text: text, Data: raw}); err != nil {
		r.t.Fatal(err)
	}
	err := r.lc.ReviewRecord("demo", service, kind, text, raw)
	r.lc.Wait()
	return err
}

func (r *reviewRig) to(service string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, d := range r.got {
		if d.service == service {
			out = append(out, d.text)
		}
	}
	return out
}

func (r *reviewRig) last(service string) string {
	all := r.to(service)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

func (r *reviewRig) review() state.Review {
	r.t.Helper()
	st := r.state("demo")
	if st.Review == nil {
		r.t.Fatal("no review in state")
	}
	return *st.Review
}

func (r *reviewRig) reviewerSpawns() int {
	n := 0
	for _, s := range r.term.Spawned() {
		if s.Service == domain.ReviewerService {
			n++
		}
	}
	return n
}

func finding(id, severity, what string) map[string]any {
	return map[string]any{"id": id, "severity": severity, "file": "src/a.ts", "line": 3, "what": what, "scenario": "empty input crashes", "fix": "guard it"}
}

func TestAReviewRoundGivesTheReviewerAPinnedCopyOfTheCodersCommit(t *testing.T) {
	r := newReviewRig(t, "{copy: [node_modules]}")
	os.MkdirAll(filepath.Join(r.coder, "node_modules", "left-pad"), 0o755)
	os.WriteFile(filepath.Join(r.coder, "node_modules", "left-pad", "index.js"), []byte("module.exports = 1\n"), 0o644)
	sha := r.commit("a.txt", "first\n")

	if err := r.record("svc1", "review.ready", "a.txt is done", nil); err != nil {
		t.Fatal(err)
	}
	rv := r.review()
	if rv.Status != domain.ReviewReviewing || rv.Round != 1 || rv.Commit != sha {
		t.Fatalf("review = %+v, want round 1 reviewing %s", rv, sha)
	}
	dir := filepath.Join(r.path, "review")
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "first\n" {
		t.Errorf("the reviewer's copy lacks the coder's commit: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "node_modules", "left-pad", "index.js")); string(got) != "module.exports = 1\n" {
		t.Errorf("node_modules was not copied: %q", got)
	}

	c, err := r.dk.InspectContainer(ctx, "rig-demo-review-agent")
	if err != nil {
		t.Fatal(err)
	}
	mounts := mountSet(c)
	if !slices.Contains(mounts, resolveDir(dir)+" -> /work rw") && !slices.Contains(mounts, dir+" -> /work rw") {
		t.Errorf("mounts = %v; the reviewer works in its own copy", mounts)
	}
	for _, m := range mounts {
		if strings.Contains(m, r.coder) {
			t.Errorf("the coder's checkout is mounted into the reviewer: %s", m)
		}
		if strings.Contains(m, "/rig/ticket/") && !strings.HasSuffix(m, " ro") {
			t.Errorf("a ticket file is writable by the reviewer: %s", m)
		}
	}
	if r.reviewerSpawns() != 1 {
		t.Fatalf("reviewer spawned %d times", r.reviewerSpawns())
	}
	var prompt string
	for _, s := range r.term.Spawned() {
		if s.Service == domain.ReviewerService {
			prompt = s.Command[len(s.Command)-1]
		}
	}
	if !strings.Contains(prompt, `reviewer of service "svc1"`) {
		t.Errorf("reviewer prompt = %q", prompt)
	}
	if brief := r.last(domain.ReviewerService); !strings.Contains(brief, "Review round 1") || !strings.Contains(brief, sha) || !strings.Contains(brief, "git diff") {
		t.Errorf("round brief = %q", brief)
	}
	if note := r.last("svc1"); !strings.Contains(note, "Review round 1 started") {
		t.Errorf("coder told %q", note)
	}

	// Another review.ready while the reviewer works is refused, and the coder told why.
	if err := r.record("svc1", "review.ready", "again", nil); err == nil || !strings.Contains(r.last("svc1"), "still running") {
		t.Errorf("a second ready: err %v, coder told %q", err, r.last("svc1"))
	}
}

func TestFindingsGoToTheCoderAndTheNextRoundStartsFresh(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "done", nil)
	r.record("review", "review.result", "empty input", finding("F1", "blocking", "crashes on empty input"))
	r.record("review", "review.result", "naming", finding("F2", "nit", "rename x"))
	if err := r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": 1}); err != nil {
		t.Fatal(err)
	}
	if rv := r.review(); rv.Status != domain.ReviewFixing || rv.Rounds[0].Verdict != "Blocking findings: 1" {
		t.Fatalf("review = %+v", rv)
	}
	msg := r.last("svc1")
	if !strings.Contains(msg, "F1 [blocking] src/a.ts:3: crashes on empty input") || !strings.Contains(msg, "Nits") || !strings.Contains(msg, "review.reply") {
		t.Errorf("coder got %q", msg)
	}

	// The reviewer planted a defect in its copy; the coder fixes and answers.
	os.WriteFile(filepath.Join(r.path, "review", "planted.txt"), []byte("x"), 0o644)
	fixed := r.commit("a.txt", "fixed\n")
	r.record("svc1", "review.reply", "", map[string]string{"id": "F1", "status": "fixed", "commit": fixed})
	if err := r.record("svc1", "review.ready", "fixed", nil); err != nil {
		t.Fatal(err)
	}
	if rv := r.review(); rv.Round != 2 || rv.Commit != fixed || rv.Status != domain.ReviewReviewing {
		t.Fatalf("review = %+v, want round 2 on %s", rv, fixed)
	}
	if _, err := os.Stat(filepath.Join(r.path, "review", "planted.txt")); err == nil {
		t.Error("the reviewer's planted file survived into round 2")
	}
	if r.reviewerSpawns() != 1 {
		t.Errorf("the reviewer was launched %d times; later rounds are typed to it", r.reviewerSpawns())
	}
	brief := r.last(domain.ReviewerService)
	if !strings.Contains(brief, "Review round 2") || !strings.Contains(brief, "F1 (blocking) crashes on empty input: fixed in "+fixed[:12]) {
		t.Errorf("round 2 brief = %q", brief)
	}

	if _, err := r.setPhase("demo", domain.PhaseTest, ""); err == nil || !strings.Contains(err.Error(), "review") {
		t.Errorf("moved out of implement before the review passed: %v", err)
	}
	r.record("review", "review.result", "naming", finding("F2", "nit", "rename x"))
	r.record("review", "verdict", "No blocking findings", map[string]int{"round": 2})
	if rv := r.review(); rv.Status != domain.ReviewPassed {
		t.Fatalf("review = %+v, want passed", rv)
	}
	if msg := r.last("svc1"); !strings.Contains(msg, "review passed in round 2") || !strings.Contains(msg, "F2") {
		t.Errorf("coder got %q", msg)
	}
	if _, err := r.setPhase("demo", domain.PhaseTest, ""); err != nil {
		t.Errorf("the move after a pass: %v", err)
	}

	status, err := r.lc.ReviewerStatus(ctx, api.ReviewerParams{Ticket: "demo"})
	if err != nil || len(status.Rounds) != 2 || len(status.Rounds[0].Findings) != 2 || status.Rounds[0].Findings[0].Reply == nil || status.Rounds[0].Findings[0].Reply.Status != "fixed" {
		t.Errorf("status = %+v, %v", status, err)
	}
}

func TestTheLoopStopsForAPersonAtACapOrADispute(t *testing.T) {
	cases := []struct {
		name, review string
		reply        map[string]string
		cost         float64
		want         string
	}{
		{"round cap", "{max_rounds: 1}", nil, 0, "round cap (1)"},
		{"budget", "{budget_usd: 2}", nil, 2.5, "budget of $2.00"},
		{"dispute", "{}", map[string]string{"id": "F1", "status": "disagree", "reason": "empty input cannot happen"}, 0, "disagrees with blocking finding F1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReviewRig(t, c.review)
			r.commit("a.txt", "first\n")
			r.record("svc1", "review.ready", "", nil)
			r.record("review", "review.result", "", finding("F1", "blocking", "crashes"))
			r.record("review", "verdict", "Blocking findings: 1", map[string]int{"round": 1})
			if c.reply != nil {
				r.record("svc1", "review.reply", "", c.reply)
			}
			if c.cost > 0 {
				r.states.Update(filepath.Join(r.path, "state.json"), func(s *state.State) error {
					ss := s.Services["svc1"]
					ss.Agent.Cost = &state.Cost{USD: c.cost}
					s.Services["svc1"] = ss
					return nil
				})
			}
			r.commit("b.txt", "more\n")
			r.record("svc1", "review.ready", "", nil)
			rv := r.review()
			if rv.Status != domain.ReviewStopped || !strings.Contains(rv.Reason, c.want) || rv.Round != 1 {
				t.Fatalf("review = %+v, want stopped for %q", rv, c.want)
			}
			if msg := r.last("svc1"); !strings.Contains(msg, "stopped") || !strings.Contains(msg, c.want) {
				t.Errorf("coder told %q", msg)
			}
			if err := r.record("svc1", "review.ready", "", nil); err == nil {
				t.Error("an agent restarted a stopped loop")
			}
			// A person can restart it, as a new loop.
			res, err := r.lc.ReviewerStart(ctx, api.ReviewerParams{Ticket: "demo"})
			if err != nil || res.Status != domain.ReviewReviewing || res.Round != 1 {
				t.Errorf("restart = %+v, %v", res, err)
			}
		})
	}
}

func TestReadyIsRefusedWithUncommittedWorkOrOutsideImplement(t *testing.T) {
	r := newReviewRig(t, "{}")
	os.WriteFile(filepath.Join(r.coder, "loose.txt"), []byte("x"), 0o644)
	if err := r.record("svc1", "review.ready", "", nil); err == nil || !strings.Contains(r.last("svc1"), "uncommitted") {
		t.Errorf("uncommitted: err %v, told %q", err, r.last("svc1"))
	}
	if st := r.state("demo"); st.Review != nil {
		t.Errorf("a refused ready changed the review: %+v", st.Review)
	}
	if err := r.record("review", "review.ready", "", nil); err == nil {
		t.Error("the reviewer started a round")
	}
	if err := r.record("svc1", "verdict", "No blocking findings", nil); err == nil {
		t.Error("the coder gave a verdict")
	}
	if err := r.record("review", "review.result", "", map[string]string{"id": "F1", "severity": "huge", "what": "x"}); err == nil {
		t.Error("a finding with an unknown severity was taken")
	}
}

func TestAPersonDecidesAndABackwardMoveStartsANewLoop(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	if _, err := r.lc.ReviewerDecide(ctx, api.ReviewerDecideParams{Ticket: "demo", Outcome: "pass"}); err == nil {
		t.Error("a decision without a reason was taken")
	}
	res, err := r.lc.ReviewerDecide(ctx, api.ReviewerDecideParams{Ticket: "demo", Outcome: "pass", Reason: "verify-only ticket"})
	if err != nil || res.Status != domain.ReviewPassed || res.By != "human" {
		t.Fatalf("decide = %+v, %v", res, err)
	}
	if !strings.Contains(r.last("svc1"), "A person passed the review") {
		t.Errorf("coder told %q", r.last("svc1"))
	}
	r.mustSet("demo", domain.PhaseTest, "")
	r.mustSet("demo", domain.PhaseImplement, "the human found a gap")
	if st := r.state("demo"); st.Review != nil {
		t.Errorf("review after a move back = %+v, want a new loop", st.Review)
	}
	if _, err := r.setPhase("demo", domain.PhaseTest, ""); err == nil {
		t.Error("work sent back left implement without a new review")
	}
}

func TestStoppingTheTicketRemovesTheReviewer(t *testing.T) {
	r := newReviewRig(t, "{}")
	r.commit("a.txt", "first\n")
	r.record("svc1", "review.ready", "", nil)
	if _, err := r.lc.Stop(ctx, api.TicketStopParams{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dk.InspectContainer(ctx, "rig-demo-review-agent"); err == nil {
		t.Error("the reviewer's container is still there")
	}
	if _, err := os.Stat(filepath.Join(r.path, "review")); !os.IsNotExist(err) {
		t.Error("the reviewer's copy is still there")
	}
	if out := r.repo.Git("for-each-ref", "refs/rig/"); out != "" {
		t.Errorf("review ref left in the main repository: %s", out)
	}
	panes, _ := r.term.List(ctx)
	for _, p := range panes {
		if p.Alive {
			t.Errorf("pane %s still alive", p.ID)
		}
	}
	if st := r.state("demo"); st.Services[domain.ReviewerService].Agent.PaneID != "" {
		t.Errorf("the reviewer's pane is still recorded: %+v", st.Services[domain.ReviewerService].Agent)
	}
}
