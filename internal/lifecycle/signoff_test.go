package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

const twoSteps = "# Tests\n\n### login: log in\ncurl x\n\n### logout: log out\ncurl y\n"

var signoffGates = []domain.Gate{domain.GatePlanApproval, domain.GateManualTestSignoff, domain.GateReviewApproval}

// signoffFixture is a single-service ticket in the test phase with two manual steps.
func signoffFixture(t *testing.T, gates []domain.Gate) *fixture {
	t.Helper()
	f := newFixtureWithGates(t, domain.SandboxContainer, gates)
	f.create("demo", testkit.NewRepo(t))
	writeManualTests(t, f, twoSteps)
	f.advanceTo("demo", domain.PhaseTest)
	return f
}

func (f *fixture) step(id string, done bool) (api.TestsStepResult, error) {
	return f.lc.TestsStep(ctx, api.TestsStepParams{Ticket: "demo", Step: id, Done: done})
}

func (f *fixture) signoff(override bool) (api.TestsSignoffResult, error) {
	return f.lc.TestsSignoff(ctx, api.TestsSignoffParams{Ticket: "demo", Override: override})
}

func TestStepsAreTickedInStateJSONAndSurviveARestart(t *testing.T) {
	f := signoffFixture(t, signoffGates)
	if res, err := f.step("login", true); err != nil || !res.Done || res.SignedOff {
		t.Fatalf("tick = %+v, %v", res, err)
	}
	st := f.state("demo")
	if !st.ManualTests.Steps["login"] || st.ManualTests.Steps["logout"] {
		t.Errorf("steps = %v", st.ManualTests.Steps)
	}
	render, err := f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "demo"})
	if err != nil || !render.Steps[0].Done || render.Steps[1].Done {
		t.Errorf("render = %+v, %v", render, err)
	}
	if _, err := f.step("login", true); err != nil { // ticking twice is fine
		t.Errorf("tick again: %v", err)
	}
	if _, err := f.step("login", false); err != nil {
		t.Fatal(err)
	}
	if f.state("demo").ManualTests.Steps["login"] {
		t.Error("the step is still ticked after unticking")
	}
}

func TestStepErrors(t *testing.T) {
	f := signoffFixture(t, signoffGates)
	if _, err := f.step("nope", true); apiCode(t, err) != api.CodeNotFound || !strings.Contains(err.Error(), "login, logout") {
		t.Errorf("unknown step = %v", err)
	}
	if _, err := f.lc.TestsStep(ctx, api.TestsStepParams{Ticket: "ghost", Step: "login", Done: true}); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("unknown ticket = %v", err)
	}
	os.Remove(filepath.Join(f.dir, "demo", "manual-tests.md"))
	if _, err := f.step("login", true); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("no file = %v", err)
	}
	writeManualTests(t, f, "### Bad_ID: x\n")
	if _, err := f.step("login", true); apiCode(t, err) != api.CodeInvalid {
		t.Errorf("malformed file = %v", err)
	}
}

func TestSigningOffNeedsEveryStepUnlessOverridden(t *testing.T) {
	f := signoffFixture(t, signoffGates)
	f.step("login", true)
	_, err := f.signoff(false)
	var apiErr *api.Error
	if apiCode(t, err) != api.CodeBlocked {
		t.Fatalf("signoff = %v, want blocked", err)
	}
	apiErr = err.(*api.Error)
	if len(apiErr.Details) != 1 || apiErr.Details[0] != "step not done: logout" || !strings.Contains(apiErr.Message, "1 of 2") {
		t.Errorf("error = %+v", apiErr)
	}
	if st := f.state("demo"); st.ManualTests.SignedOff || len(st.Approvals) != 1 { // only the plan approval
		t.Errorf("a refused sign-off changed state: %+v", st)
	}

	res, err := f.signoff(true)
	if err != nil || !res.SignedOff || !res.Approved || len(res.Unchecked) != 1 || res.Unchecked[0] != "logout" {
		t.Fatalf("override = %+v, %v", res, err)
	}
}

func TestSigningOffRecordsTheGateApprovalAndUnlocksTheMove(t *testing.T) {
	f := signoffFixture(t, signoffGates)
	if _, err := f.setPhase("demo", domain.PhaseReview, ""); apiCode(t, err) != api.CodeBlocked {
		t.Fatalf("moving before the sign-off = %v", err)
	}
	f.step("login", true)
	f.step("logout", true)
	f.events()
	res, err := f.signoff(false)
	if err != nil || !res.SignedOff || !res.Approved || len(res.Unchecked) != 0 {
		t.Fatalf("signoff = %+v, %v", res, err)
	}
	st := f.state("demo")
	var approval bool
	for _, a := range st.Approvals {
		approval = approval || a.Gate == domain.GateManualTestSignoff && a.By == "human" && a.Service == ""
	}
	if !st.ManualTests.SignedOff || !approval {
		t.Errorf("state = %+v", st)
	}
	var announced bool
	for _, ev := range f.events() {
		announced = announced || ev.Type == api.EventGateApproved
	}
	if !announced {
		t.Error("no gate.approved event")
	}
	// signing off again changes nothing and publishes nothing
	f.events()
	if again, err := f.signoff(false); err != nil || again.Approved || len(f.events()) != 0 {
		t.Errorf("second sign-off = %+v, %v", again, err)
	}
	f.mustSet("demo", domain.PhaseReview, "")
}

func TestUntickingAStepWithdrawsTheSignoff(t *testing.T) {
	f := signoffFixture(t, signoffGates)
	f.step("login", true)
	f.step("logout", true)
	f.signoff(false)
	res, err := f.step("logout", false)
	if err != nil || res.SignedOff {
		t.Fatalf("untick = %+v, %v", res, err)
	}
	st := f.state("demo")
	if st.ManualTests.SignedOff {
		t.Error("still signed off")
	}
	for _, a := range st.Approvals {
		if a.Gate == domain.GateManualTestSignoff {
			t.Error("the sign-off approval survived")
		}
	}
	if _, err := f.setPhase("demo", domain.PhaseReview, ""); apiCode(t, err) != api.CodeBlocked {
		t.Errorf("moving after the withdrawal = %v", err)
	}
	// ticking another step on does not touch an unsigned ticket's approvals
	if res, _ := f.step("logout", true); res.SignedOff {
		t.Error("ticking signed the ticket off")
	}
}

func TestSignoffWithoutTheGateRequiredStillRecordsTheSignoff(t *testing.T) {
	f := signoffFixture(t, strict) // plan and review only
	f.step("login", true)
	f.step("logout", true)
	res, err := f.signoff(false)
	if err != nil || !res.SignedOff || res.Approved {
		t.Errorf("signoff = %+v, %v; no service requires the gate, so nothing is approved", res, err)
	}
	if !f.state("demo").ManualTests.SignedOff {
		t.Error("not signed off")
	}
}

func TestSignoffNeedsTheTestPhaseAndSomethingToSignOff(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, signoffGates)
	f.create("demo", testkit.NewRepo(t))
	writeManualTests(t, f, twoSteps)
	if _, err := f.signoff(false); apiCode(t, err) != api.CodeConflict || !strings.Contains(err.Error(), "test phase") {
		t.Errorf("in plan = %v", err)
	}
	f.advanceTo("demo", domain.PhaseTest)
	writeManualTests(t, f, "# nothing here\n")
	_, err := f.signoff(false)
	if apiCode(t, err) != api.CodeBlocked || !strings.Contains(err.Error(), "no steps") {
		t.Errorf("no steps = %v", err)
	}
	if res, err := f.signoff(true); err != nil || !res.SignedOff {
		t.Errorf("override with no steps = %+v, %v", res, err)
	}
	os.Remove(filepath.Join(f.dir, "demo", "manual-tests.md"))
	if _, err := f.signoff(true); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("no file = %v", err)
	}
}
