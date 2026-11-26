package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
	tline "github.com/charlesrobsampson/rig/internal/timeline"
)

func (f *fixture) summary(id string) api.TicketSummary {
	f.t.Helper()
	res, err := registry.New(f.dir, f.states).Get(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return res.Summary
}

func TestAQuestionShowsOnTheSummaryUntilAPersonAnswersTheServiceThatAsked(t *testing.T) {
	f := newFixture(t, domain.SandboxHost)
	path := f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))

	if err := f.lc.AskQuestion("demo", "svc1", "  Keep the old flag?  "); err != nil {
		t.Fatal(err)
	}
	q := f.summary("demo").Question
	if q == nil || q.Service != "svc1" || q.Text != "Keep the old flag?" || q.At.IsZero() {
		t.Fatalf("question = %+v", q)
	}

	if err := f.lc.AnswerQuestion("demo", "svc2"); err != nil { // another service: not an answer
		t.Fatal(err)
	}
	if f.summary("demo").Question == nil {
		t.Fatal("a message to another service answered the question")
	}
	if err := f.lc.AskQuestion("demo", "svc1", "Or drop it?"); err != nil {
		t.Fatal(err)
	}
	if q := f.summary("demo").Question; q == nil || q.Text != "Or drop it?" {
		t.Fatalf("a newer question did not replace the older: %+v", q)
	}
	if err := f.lc.AnswerQuestion("demo", "svc1"); err != nil {
		t.Fatal(err)
	}
	if q := f.summary("demo").Question; q != nil {
		t.Fatalf("an answer left the question open: %+v", q)
	}
	_ = f.lc.AnswerQuestion("demo", "svc1") // nothing open: nothing recorded

	entries := readTimeline(t, path)
	if got := strings.Join(kindsOf(entries), ","); got != "question.asked,question.asked,question.answered" {
		t.Fatalf("timeline = %s", got)
	}
	if e := entries[0]; e.By != "agent:svc1" || e.Service != "svc1" || string(e.Data) != `{"text":"Keep the old flag?"}` {
		t.Errorf("asked = %+v (%s)", e, e.Data)
	}
	if e := entries[2]; e.By != "human" || e.Service != "svc1" {
		t.Errorf("answered = %+v", e)
	}
}

func TestABlankQuestionOrOneFromANonServiceIsRefused(t *testing.T) {
	f := newFixture(t, domain.SandboxHost)
	f.create("demo", testkit.NewRepo(t))
	if err := f.lc.AskQuestion("demo", "svc1", "   "); err == nil {
		t.Error("a blank question was taken")
	}
	if err := f.lc.AskQuestion("demo", "ghost", "hi?"); err == nil {
		t.Error("a question from an unknown service was taken")
	}
	if f.summary("demo").Question != nil {
		t.Error("a refused question was recorded")
	}
}

func (f *fixture) rework(id, finding, lens string) (api.TransitionResult, error) {
	return f.lc.Rework(ctx, api.ReworkParams{ID: id, Finding: finding, Lens: lens})
}

func TestReworkFromTestReviewAndPushMovesBackTellsTheCoderAndRecordsTheMiss(t *testing.T) {
	for _, from := range []domain.Phase{domain.PhaseTest, domain.PhaseReview, domain.PhasePush} {
		for _, lens := range []string{"security", ""} {
			t.Run(string(from)+"/"+lens, func(t *testing.T) {
				f := newFixture(t, domain.SandboxContainer)
				var told []string
				f.lc = lifecycle.New(lifecycle.Options{
					Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch), Global: config.Global{Defaults: config.Defaults{BranchPrefix: "rig/"}},
					OnPhaseChanged: func(_ string, notices map[string]string) {
						for svc, n := range notices {
							told = append(told, svc+": "+n)
						}
					},
				})
				path := f.create("demo", testkit.NewRepo(t))
				f.states.Update(filepath.Join(path, "state.json"), func(s *state.State) error {
					s.Services["svc1"] = state.ServiceState{Agent: state.Agent{PaneID: "p1", State: domain.AgentIdle}}
					return nil
				})
				f.advanceTo("demo", from)
				told = nil

				res, err := f.rework("demo", "  the retry loop never stops  ", lens)
				if err != nil {
					t.Fatal(err)
				}
				if res.From != from || res.To != domain.PhaseImplement || res.By != "human" || res.Reason != "the retry loop never stops" {
					t.Errorf("result = %+v", res)
				}
				if got := f.state("demo").Phase; got != domain.PhaseImplement {
					t.Errorf("phase = %s", got)
				}
				if len(told) != 1 || !strings.HasPrefix(told[0], "svc1: ") || !strings.Contains(told[0], "the retry loop never stops") {
					t.Errorf("told = %q", told)
				}
				missed := only(readTimeline(t, path), tline.ReviewMissed)
				if len(missed) != 1 || missed[0]["finding"] != "the retry loop never stops" {
					t.Fatalf("missed = %+v", missed)
				}
				if l, _ := missed[0]["lens"].(string); l != lens {
					t.Errorf("lens = %q, want %q", l, lens)
				}
				moved := only(readTimeline(t, path), tline.PhaseMoved)
				if last := moved[len(moved)-1]; last["to"] != "implement" || last["reason"] != "the retry loop never stops" {
					t.Errorf("phase.moved = %+v", last)
				}
			})
		}
	}
}

func TestReworkIsRefusedFromPlanImplementAndDoneAndRecordsNothing(t *testing.T) {
	for _, from := range []domain.Phase{domain.PhasePlan, domain.PhaseImplement, domain.PhaseDone} {
		t.Run(string(from), func(t *testing.T) {
			f := newFixture(t, domain.SandboxHost)
			path := f.create("demo", testkit.NewRepo(t))
			if from != domain.PhasePlan {
				f.advanceTo("demo", from)
			}
			before := len(readTimeline(t, path))
			if _, err := f.rework("demo", "missed it", "security"); err == nil || !strings.Contains(err.Error(), "from test, review or push") {
				t.Fatalf("err = %v", err)
			}
			if got := f.state("demo").Phase; got != from {
				t.Errorf("phase = %s, want %s", got, from)
			}
			if n := len(readTimeline(t, path)); n != before {
				t.Errorf("a refused rework recorded %d entries", n-before)
			}
		})
	}
}

func TestReworkNeedsAFindingAndNamesTheReviewedServiceOfAReviewedTicket(t *testing.T) {
	f := newFixture(t, domain.SandboxHost)
	path := f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	f.advanceTo("demo", domain.PhaseTest)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("review:\n  of: svc2\n")...), 0o600)
	if _, err := f.rework("demo", "  ", ""); err == nil {
		t.Fatal("a blank finding was taken")
	}
	if _, err := f.rework("demo", "missed", ""); err != nil {
		t.Fatal(err)
	}
	if e := only(readTimeline(t, path), tline.ReviewMissed); len(e) != 1 {
		t.Fatalf("missed = %+v", e)
	}
	entries := readTimeline(t, path)
	if e := entries[len(entries)-1]; e.Kind != tline.ReviewMissed || e.Service != "svc2" {
		t.Errorf("last = %+v", e)
	}
}

func TestAnAnswerThatCannotBeRecordedIsReturned(t *testing.T) {
	f := newFixture(t, domain.SandboxHost)
	if err := f.lc.AnswerQuestion("nope", "svc1"); err == nil {
		t.Error("an unknown ticket was silently ignored")
	}
}
