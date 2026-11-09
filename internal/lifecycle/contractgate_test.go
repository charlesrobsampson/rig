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

// agree writes something under the ticket's "## Agreed" heading.
func agree(t *testing.T, dir string) {
	t.Helper()
	md := "# Contract: demo\n\n## Agreed\nGET /api/v1/x returns {\"ok\": true}.\n\n## Proposed changes\n\n## Changelog\n"
	if err := os.WriteFile(filepath.Join(dir, "contract.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestApprovingThePlanNeedsAnAgreedContractForMultiServiceTickets(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	dir := bareTwoServiceTicket(t, f, "demo")

	_, err := f.approve("demo", domain.GatePlanApproval, "")
	var apiErr *api.Error
	if apiCode(t, err) != api.CodeBlocked || !strings.Contains(err.Error(), "contract") {
		t.Fatalf("error = %v, want blocked mentioning the contract", err)
	}
	apiErr = err.(*api.Error)
	if len(apiErr.Details) == 0 || !strings.Contains(apiErr.Details[0], "## Agreed") {
		t.Errorf("details = %v; they should say what to do", apiErr.Details)
	}
	if res, _ := f.gates("demo"); statusOf(res, domain.GatePlanApproval, "svc1").Approved {
		t.Error("the refused approval was recorded")
	}

	agree(t, dir)
	f.mustApprove("demo", domain.GatePlanApproval, "")
	f.mustSet("demo", domain.PhaseImplement, "")
}

func TestGuidanceCommentsAndWhitespaceCountAsEmpty(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	dir := bareTwoServiceTicket(t, f, "demo")
	os.WriteFile(filepath.Join(dir, "contract.md"), []byte("# C\n\n## Agreed\n<!-- guidance -->\n\n   \n## Proposed changes\n"), 0o600)
	if _, err := f.approve("demo", domain.GatePlanApproval, ""); apiCode(t, err) != api.CodeBlocked {
		t.Errorf("comments only: %v", err)
	}
	os.Remove(filepath.Join(dir, "contract.md"))
	if _, err := f.approve("demo", domain.GatePlanApproval, ""); apiCode(t, err) != api.CodeBlocked || !strings.Contains(err.Error(), "missing") {
		t.Errorf("no file: %v", err)
	}
}

func TestSingleServiceTicketsNeedNoContract(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	f.create("solo", testkit.NewRepo(t))
	f.mustApprove("solo", domain.GatePlanApproval, "")
	f.mustSet("solo", domain.PhaseImplement, "")
}

func TestTheContractIsNotRequiredWhenNoServiceRequiresThePlanGate(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, nil)
	bareTwoServiceTicket(t, f, "demo")
	f.mustSet("demo", domain.PhaseImplement, "")
}

func TestAnApprovedPlanDoesNotLetAnEmptiedContractThrough(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	dir := twoServiceTicket(t, f, "demo")
	agree(t, dir)
	f.mustApprove("demo", domain.GatePlanApproval, "")
	os.WriteFile(filepath.Join(dir, "contract.md"), []byte("# C\n\n## Agreed\n\n## Proposed changes\n"), 0o600) // wiped after approval
	if _, err := f.setPhase("demo", domain.PhaseImplement, ""); apiCode(t, err) != api.CodeBlocked {
		t.Errorf("moving with an emptied contract = %v", err)
	}
	if st := f.state("demo"); st.Phase != domain.PhasePlan {
		t.Errorf("phase = %s", st.Phase)
	}
}
