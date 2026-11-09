package lifecycle_test

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func (f *fixture) setPhase(id string, to domain.Phase, reason string) (api.TransitionResult, error) {
	return f.lc.SetPhase(ctx, api.PhaseSetParams{ID: id, To: to, Reason: reason})
}

func (f *fixture) requestPhase(id, service string, to domain.Phase, reason string) (api.TransitionResult, error) {
	return f.lc.RequestTransition(ctx, api.TransitionRequestParams{ID: id, Service: service, To: to, Reason: reason})
}

func (f *fixture) mustSet(id string, to domain.Phase, reason string) api.TransitionResult {
	f.t.Helper()
	res, err := f.setPhase(id, to, reason)
	if err != nil {
		f.t.Fatalf("move to %s: %v", to, err)
	}
	return res
}

// advance walks a ticket forward, approving whatever gate stands in the way.
func (f *fixture) advanceTo(id string, target domain.Phase) {
	f.t.Helper()
	for _, step := range []struct {
		to   domain.Phase
		gate domain.Gate
	}{
		{domain.PhaseImplement, domain.GatePlanApproval},
		{domain.PhaseTest, ""},
		{domain.PhaseReview, ""},
		{domain.PhasePush, domain.GateReviewApproval},
		{domain.PhaseDone, ""},
	} {
		if step.gate != "" && f.requires(id, step.gate) {
			f.mustApprove(id, step.gate, "")
		}
		f.mustSet(id, step.to, "")
		if step.to == target {
			return
		}
	}
}

// requires reports whether any service of the ticket requires the gate.
func (f *fixture) requires(id string, gate domain.Gate) bool {
	res, err := f.gates(id)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, g := range res.Gates {
		if g.Gate == gate {
			return true
		}
	}
	return false
}

func apiError(t *testing.T, err error) *api.Error {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want an *api.Error", err)
	}
	return e
}

func TestAHumanMovesAFullyApprovedTicketForward(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	twoServiceTicket(t, f, "demo")
	f.mustApprove("demo", domain.GatePlanApproval, "")
	f.events()

	res := f.mustSet("demo", domain.PhaseImplement, "")
	if res.From != domain.PhasePlan || res.To != domain.PhaseImplement || res.By != "human" || !res.At.Equal(epoch) || res.Summary.Phase != domain.PhaseImplement {
		t.Errorf("result = %+v", res)
	}
	st := f.state("demo")
	if st.Phase != domain.PhaseImplement || len(st.Transitions) != 1 {
		t.Fatalf("state = phase %s, %d transitions", st.Phase, len(st.Transitions))
	}
	if got := st.Transitions[0]; got.From != domain.PhasePlan || got.To != domain.PhaseImplement || got.By != "human" || got.Reason != "" || !got.At.Equal(epoch) {
		t.Errorf("logged transition = %+v", got)
	}

	evs := f.events()
	if len(evs) != 1 || evs[0].Type != api.EventPhaseChanged {
		t.Fatalf("events = %+v, want exactly one phase.changed", evs)
	}
	var data api.PhaseChangedData
	if err := evs[0].DecodeData(&data); err != nil || data.From != domain.PhasePlan || data.To != domain.PhaseImplement || data.By != "human" {
		t.Errorf("event data = %+v, %v", data, err)
	}
}

func TestAMissingApprovalBlocksAndSaysWhichOnes(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	twoServiceTicket(t, f, "demo")
	f.events()

	_, err := f.setPhase("demo", domain.PhaseImplement, "")
	e := apiError(t, err)
	if e.Code != api.CodeBlocked || !strings.Contains(e.Message, "rig approve demo plan_approval") {
		t.Fatalf("error = %v, want a blocked error that says how to approve", err)
	}
	if want := []string{"plan_approval: svc1", "plan_approval: svc2"}; !reflect.DeepEqual(e.Details, want) {
		t.Errorf("details = %v, want %v", e.Details, want)
	}
	if st := f.state("demo"); st.Phase != domain.PhasePlan || len(st.Transitions) != 0 {
		t.Errorf("a refused move changed the state: phase %s, %d transitions", st.Phase, len(st.Transitions))
	}
	if evs := f.events(); len(evs) != 0 {
		t.Errorf("a refused move published %d event(s)", len(evs))
	}

	f.mustApprove("demo", domain.GatePlanApproval, "svc1")
	_, err = f.setPhase("demo", domain.PhaseImplement, "")
	if e := apiError(t, err); e.Code != api.CodeBlocked || !reflect.DeepEqual(e.Details, []string{"plan_approval: svc2"}) {
		t.Errorf("after one approval: error = %v, want only svc2 listed", err)
	}
	f.mustApprove("demo", domain.GatePlanApproval, "svc2")
	f.mustSet("demo", domain.PhaseImplement, "")
}

func TestUngatedStepsNeedNoApproval(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	f.create("demo", testkit.NewRepo(t))
	f.advanceTo("demo", domain.PhaseImplement)
	f.mustSet("demo", domain.PhaseTest, "")   // implement -> test: no gate
	f.mustSet("demo", domain.PhaseReview, "") // test -> review: manual_test_signoff is not required by this policy
	if _, err := f.setPhase("demo", domain.PhasePush, ""); apiError(t, err).Code != api.CodeBlocked {
		t.Errorf("review -> push must wait for review_approval, got %v", err)
	}
}

func TestTheOrderOfPhasesIsEnforced(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, nil)
	f.create("demo", testkit.NewRepo(t))
	tests := []struct {
		name string
		to   domain.Phase
		text string
	}{
		{"skipping a phase", domain.PhaseTest, "one step"},
		{"staying where it is", domain.PhasePlan, "already"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.setPhase("demo", tt.to, "")
			if e := apiError(t, err); e.Code != api.CodeConflict || !strings.Contains(e.Message, tt.text) {
				t.Errorf("error = %v, want a conflict mentioning %q", err, tt.text)
			}
		})
	}

	f.advanceTo("demo", domain.PhaseDone)
	if _, err := f.setPhase("demo", domain.PhasePlan, "again"); apiError(t, err).Code != api.CodeConflict || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("leaving done = %v, want a conflict about the terminal phase", err)
	}
}

func TestMovingBackwardNeedsAReasonAndRedoesApprovals(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	f.create("demo", testkit.NewRepo(t))
	f.advanceTo("demo", domain.PhasePush) // plan, review approvals given along the way
	f.events()

	if _, err := f.setPhase("demo", domain.PhaseImplement, ""); apiError(t, err).Code != api.CodeInvalid || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("a backward move without a reason = %v, want an invalid error about the reason", err)
	}
	if _, err := f.setPhase("demo", domain.PhaseImplement, "   "); apiError(t, err).Code != api.CodeInvalid {
		t.Error("a blank reason is no reason")
	}
	if f.state("demo").Phase != domain.PhasePush {
		t.Fatal("a refused backward move changed the phase")
	}

	res := f.mustSet("demo", domain.PhaseImplement, "review found a design flaw")
	if res.Reason != "review found a design flaw" {
		t.Errorf("result reason = %q", res.Reason)
	}
	st := f.state("demo")
	last := st.Transitions[len(st.Transitions)-1]
	if last.From != domain.PhasePush || last.To != domain.PhaseImplement || last.Reason != "review found a design flaw" {
		t.Errorf("logged = %+v", last)
	}
	var kept []domain.Gate
	for _, a := range st.Approvals {
		kept = append(kept, a.Gate)
	}
	if !reflect.DeepEqual(kept, []domain.Gate{domain.GatePlanApproval}) {
		t.Errorf("approvals after moving back to implement = %v; only plan_approval should still hold (review_approval was for work now being redone)", kept)
	}

	// Going forward again needs the review approval anew, but not the plan one.
	f.mustSet("demo", domain.PhaseTest, "")
	f.mustSet("demo", domain.PhaseReview, "")
	if _, err := f.setPhase("demo", domain.PhasePush, ""); apiError(t, err).Code != api.CodeBlocked {
		t.Errorf("review -> push after a redo = %v, want blocked until review is approved again", err)
	}
}

func TestMovingBackToPlanForgetsEveryApproval(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	f.create("demo", testkit.NewRepo(t))
	f.advanceTo("demo", domain.PhaseImplement)
	f.mustSet("demo", domain.PhasePlan, "scope change")
	if n := len(f.state("demo").Approvals); n != 0 {
		t.Errorf("%d approval(s) survived a return to plan", n)
	}
	if _, err := f.setPhase("demo", domain.PhaseImplement, ""); apiError(t, err).Code != api.CodeBlocked {
		t.Error("the plan must be approved again after a scope change")
	}
}

func TestAnAgentRequestFollowsExactlyTheHumansRules(t *testing.T) {
	type outcome struct {
		code    api.ErrorCode
		message string
		details []string
		phase   domain.Phase
	}
	run := func(t *testing.T, move func(f *fixture, to domain.Phase, reason string) error, script func(f *fixture, move func(domain.Phase, string) error) []outcome) []outcome {
		f := newFixtureWithGates(t, domain.SandboxContainer, strict)
		twoServiceTicket(t, f, "demo")
		return script(f, func(to domain.Phase, reason string) error { return move(f, to, reason) })
	}
	script := func(f *fixture, move func(domain.Phase, string) error) []outcome {
		var out []outcome
		record := func(err error) {
			o := outcome{phase: f.state("demo").Phase}
			if err != nil {
				e := apiError(f.t, err)
				o.code, o.message, o.details = e.Code, e.Message, e.Details
			}
			out = append(out, o)
		}
		record(move(domain.PhaseImplement, "")) // blocked: nothing approved
		record(move(domain.PhaseTest, ""))      // skips a phase
		f.mustApprove("demo", domain.GatePlanApproval, "")
		record(move(domain.PhaseImplement, "")) // allowed
		record(move(domain.PhaseImplement, "")) // already there
		record(move(domain.PhasePlan, ""))      // backward without a reason
		record(move(domain.PhasePlan, "redo"))  // backward with a reason
		return out
	}
	human := run(t, func(f *fixture, to domain.Phase, reason string) error {
		_, err := f.setPhase("demo", to, reason)
		return err
	}, script)
	agent := run(t, func(f *fixture, to domain.Phase, reason string) error {
		_, err := f.requestPhase("demo", "svc1", to, reason)
		return err
	}, script)
	if !reflect.DeepEqual(human, agent) {
		t.Errorf("a human and an agent got different outcomes:\nhuman: %+v\nagent: %+v", human, agent)
	}
	if human[2].code != "" || human[0].code != api.CodeBlocked || human[1].code != api.CodeConflict || human[4].code != api.CodeInvalid || human[5].code != "" {
		t.Errorf("outcomes = %+v; the script no longer exercises the cases it means to", human)
	}
}

func TestAnAgentRequestIsLoggedAsTheAgent(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	twoServiceTicket(t, f, "demo")
	f.mustApprove("demo", domain.GatePlanApproval, "")
	f.events()

	res, err := f.requestPhase("demo", "svc2", domain.PhaseImplement, "plan approved, starting work")
	if err != nil || res.By != "agent:svc2" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got := f.state("demo").Transitions[0]; got.By != "agent:svc2" || got.Reason != "plan approved, starting work" {
		t.Errorf("logged = %+v", got)
	}
	evs := f.events()
	var data api.PhaseChangedData
	if len(evs) != 1 || evs[0].DecodeData(&data) != nil || data.By != "agent:svc2" {
		t.Errorf("events = %+v, want phase.changed attributed to the agent", evs)
	}

	_, err = f.requestPhase("demo", "ghost", domain.PhaseTest, "")
	if e := apiError(t, err); e.Code != api.CodeInvalid || !strings.Contains(e.Message, "ghost") {
		t.Errorf("a request from an unknown service = %v", err)
	}
	if _, err := f.requestPhase("demo", "", domain.PhaseTest, ""); apiError(t, err).Code != api.CodeInvalid {
		t.Error("a request must say which service's agent is asking")
	}
}

func TestAnAgentCanNeverGrantAGate(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	f.create("demo", testkit.NewRepo(t))
	// Asking to move forward does not approve anything on the way.
	if _, err := f.requestPhase("demo", "svc1", domain.PhaseImplement, "please"); apiError(t, err).Code != api.CodeBlocked {
		t.Errorf("error = %v, want blocked", err)
	}
	if n := len(f.state("demo").Approvals); n != 0 {
		t.Errorf("%d approval(s) appeared out of a request", n)
	}
}

func TestTheTransitionLogIsOrderedAndAppendOnly(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	f.create("demo", testkit.NewRepo(t))
	f.advanceTo("demo", domain.PhaseReview)
	f.mustSet("demo", domain.PhaseTest, "found a bug")
	f.mustSet("demo", domain.PhaseReview, "")

	var path []string
	for _, tr := range f.state("demo").Transitions {
		path = append(path, string(tr.From)+">"+string(tr.To))
	}
	want := []string{"plan>implement", "implement>test", "test>review", "review>test", "test>review"}
	if !reflect.DeepEqual(path, want) {
		t.Errorf("log = %v, want %v", path, want)
	}
}

func TestTransitionErrors(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	f.create("demo", testkit.NewRepo(t))
	tests := []struct {
		name string
		fn   func() error
		code api.ErrorCode
	}{
		{"an unknown ticket", func() error { _, err := f.setPhase("nope", domain.PhaseImplement, ""); return err }, api.CodeNotFound},
		{"an invalid id", func() error { _, err := f.setPhase("../etc", domain.PhaseImplement, ""); return err }, api.CodeInvalid},
		{"a request on an unknown ticket", func() error { _, err := f.requestPhase("nope", "svc1", domain.PhaseImplement, ""); return err }, api.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); apiError(t, err).Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

func TestConcurrentMovesToTheSamePhaseHaveOneWinner(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, nil)
	f.create("demo", testkit.NewRepo(t))
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.setPhase("demo", domain.PhaseImplement, "")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if st := f.state("demo"); wins != 1 || len(st.Transitions) != 1 {
		t.Errorf("%d moves succeeded and %d were logged, want exactly one of each", wins, len(st.Transitions))
	}
}
