package lifecycle_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var strict = []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval}

func (f *fixture) gates(id string) (api.GateListResult, error) {
	return f.lc.Gates(ctx, api.GateListParams{ID: id})
}

func (f *fixture) approve(id string, gate domain.Gate, service string) (api.GateListResult, error) {
	return f.lc.Approve(ctx, api.GateApproveParams{ID: id, Gate: gate, Service: service})
}

func (f *fixture) mustApprove(id string, gate domain.Gate, service string) api.GateListResult {
	f.t.Helper()
	res, err := f.approve(id, gate, service)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// twoServiceTicket is a two-service ticket whose services have agreed a contract, so
// the plan can be approved; bareTwoServiceTicket leaves the contract empty.
func twoServiceTicket(t *testing.T, f *fixture, id string) string {
	t.Helper()
	dir := bareTwoServiceTicket(t, f, id)
	agree(t, dir)
	return dir
}

func bareTwoServiceTicket(t *testing.T, f *fixture, id string) string {
	t.Helper()
	return f.create(id, testkit.NewRepo(t), testkit.NewRepo(t))
}

func statusOf(res api.GateListResult, gate domain.Gate, service string) *api.GateStatus {
	for i := range res.Gates {
		if res.Gates[i].Gate == gate && res.Gates[i].Service == service {
			return &res.Gates[i]
		}
	}
	return nil
}

func TestGatesListsWhatEachServiceRequires(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	twoServiceTicket(t, f, "demo")

	res, err := f.gates("demo")
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != domain.PhasePlan || len(res.Gates) != 4 {
		t.Fatalf("result = %+v, want 4 requirements (2 gates x 2 services)", res)
	}
	plan := statusOf(res, domain.GatePlanApproval, "svc1")
	if plan == nil || plan.Approved || !plan.Current || plan.From != domain.PhasePlan || plan.To != domain.PhaseImplement {
		t.Errorf("plan gate = %+v; it guards plan -> implement and is the current gate", plan)
	}
	review := statusOf(res, domain.GateReviewApproval, "svc2")
	if review == nil || review.Current || review.From != domain.PhaseReview || review.To != domain.PhasePush {
		t.Errorf("review gate = %+v; it is not the current gate while the ticket is in plan", review)
	}
}

func TestApprovingOneServiceLeavesTheOtherPending(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	dir := twoServiceTicket(t, f, "demo")
	f.events()

	res := f.mustApprove("demo", domain.GatePlanApproval, "svc1")
	if got := statusOf(res, domain.GatePlanApproval, "svc1"); got == nil || !got.Approved || got.By != "human" || got.At == nil || !got.At.Equal(epoch) {
		t.Errorf("svc1 = %+v, want approved by human at the clock's time", got)
	}
	if got := statusOf(res, domain.GatePlanApproval, "svc2"); got == nil || got.Approved {
		t.Errorf("svc2 = %+v, want still pending", got)
	}

	evs := f.events()
	if len(evs) != 1 || evs[0].Type != api.EventGateApproved || evs[0].Ticket != "demo" {
		t.Fatalf("events = %+v, want one gate.approved", evs)
	}
	var data api.GateApprovedData
	if err := evs[0].DecodeData(&data); err != nil || data.Gate != domain.GatePlanApproval || data.Service != "svc1" {
		t.Errorf("event data = %+v, %v", data, err)
	}

	st, err := state.NewManager(testkit.NewFakeClock(epoch)).Load(filepath.Join(dir, "state.json"))
	if err != nil || len(st.Approvals) != 1 || st.Approvals[0].Service != "svc1" || st.Approvals[0].By != "human" {
		t.Errorf("state approvals = %+v, %v", st.Approvals, err)
	}
}

func TestApprovingWithoutAServiceCoversThemAll(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	twoServiceTicket(t, f, "demo")
	res := f.mustApprove("demo", domain.GatePlanApproval, "")
	for _, svc := range []string{"svc1", "svc2"} {
		if got := statusOf(res, domain.GatePlanApproval, svc); got == nil || !got.Approved {
			t.Errorf("%s = %+v, want approved by the blanket approval", svc, got)
		}
	}
	if got := statusOf(res, domain.GateReviewApproval, "svc1"); got.Approved {
		t.Error("approving one gate must not approve another")
	}
}

func TestAServiceWithNoGatesNeedsNothing(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	a, b := testkit.NewRepo(t), testkit.NewRepo(t)
	dir := f.create("mixed", a, b)
	yaml := "id: mixed\ntitle: Mixed\nservices:\n" +
		"  - {name: svc1, repo: " + a.Dir + ", branch: rig/mixed/svc1}\n" +
		"  - {name: greenfield, repo: " + b.Dir + ", branch: rig/mixed/greenfield, policy: {gates: []}}\n"
	os.WriteFile(filepath.Join(dir, "ticket.yaml"), []byte(yaml), 0o600)

	res, _ := f.gates("mixed")
	if statusOf(res, domain.GatePlanApproval, "greenfield") != nil || statusOf(res, domain.GatePlanApproval, "svc1") == nil {
		t.Errorf("gates = %+v; only the gated service should be listed", res.Gates)
	}
	_, err := f.approve("mixed", domain.GatePlanApproval, "greenfield")
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeInvalid || !strings.Contains(e.Message, "greenfield") {
		t.Errorf("error = %v, want invalid: that service does not require the gate", err)
	}
}

func TestApprovalMistakesAreRefused(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	twoServiceTicket(t, f, "demo")
	tests := []struct {
		name    string
		gate    domain.Gate
		service string
		code    api.ErrorCode
		text    string
	}{
		{"a gate no service requires", domain.GateManualTestSignoff, "", api.CodeInvalid, "not required"},
		{"a gate for a later phase", domain.GateReviewApproval, "", api.CodeConflict, "plan"},
		{"a service that does not exist", domain.GatePlanApproval, "ghost", api.CodeInvalid, "ghost"},
		{"no gate named", "", "", api.CodeInvalid, "gate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.approve("demo", tt.gate, tt.service)
			var e *api.Error
			if !errors.As(err, &e) || e.Code != tt.code || !strings.Contains(e.Message, tt.text) {
				t.Errorf("error = %v, want %s mentioning %q", err, tt.code, tt.text)
			}
		})
	}
	if res, _ := f.gates("demo"); statusOf(res, domain.GatePlanApproval, "svc1").Approved {
		t.Error("a refused approval was recorded")
	}
}

func TestApprovingTwiceChangesNothing(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	dir := twoServiceTicket(t, f, "demo")
	f.mustApprove("demo", domain.GatePlanApproval, "")
	f.events()

	f.mustApprove("demo", domain.GatePlanApproval, "")
	f.mustApprove("demo", domain.GatePlanApproval, "svc1") // already covered by the blanket approval
	st, _ := state.NewManager(testkit.NewFakeClock(epoch)).Load(filepath.Join(dir, "state.json"))
	if len(st.Approvals) != 1 {
		t.Errorf("approvals = %+v, want the single original", st.Approvals)
	}
	if evs := f.events(); len(evs) != 0 {
		t.Errorf("a repeated approval published %d event(s)", len(evs))
	}
}

func TestApprovalsSurviveARestart(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	twoServiceTicket(t, f, "demo")
	f.mustApprove("demo", domain.GatePlanApproval, "svc2")

	// A new daemon: new managers over the same files.
	clock := testkit.NewFakeClock(epoch)
	lc := lifecycle.New(lifecycle.Options{Dir: f.dir, States: state.NewManager(clock), Global: config.Global{Defaults: config.Defaults{Policy: domain.Policy{Gates: strict}}}, Clock: clock})
	res, err := lc.Gates(ctx, api.GateListParams{ID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(res, domain.GatePlanApproval, "svc2"); got == nil || !got.Approved {
		t.Errorf("svc2 = %+v, want the approval to survive", got)
	}
	if got := statusOf(res, domain.GatePlanApproval, "svc1"); got.Approved {
		t.Error("svc1 gained an approval it never had")
	}
}

func TestGateErrors(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	dir := twoServiceTicket(t, f, "demo")
	if err := f.states.Update(filepath.Join(dir, "state.json"), func(s *state.State) error { s.Phase = domain.PhaseDone; return nil }); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		fn   func() error
		code api.ErrorCode
	}{
		{"listing an unknown ticket", func() error { _, err := f.gates("nope"); return err }, api.CodeNotFound},
		{"approving on an unknown ticket", func() error { _, err := f.approve("nope", domain.GatePlanApproval, ""); return err }, api.CodeNotFound},
		{"an invalid id", func() error { _, err := f.gates("../etc"); return err }, api.CodeInvalid},
		{"approving on a finished ticket", func() error { _, err := f.approve("demo", domain.GatePlanApproval, ""); return err }, api.CodeConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e *api.Error
			if err := tt.fn(); !errors.As(err, &e) || e.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}
