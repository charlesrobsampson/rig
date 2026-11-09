package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/contract"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

type resolution struct{ ticket, id, outcome, reason string }

// contractFixture is a two-service ticket whose contract holds two open proposals.
func contractFixture(t *testing.T) (*fixture, string, *[]resolution) {
	t.Helper()
	f := newFixture(t, domain.SandboxContainer)
	var got []resolution
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch), Global: config.Global{Defaults: config.Defaults{BranchPrefix: "rig/"}},
		OnContractResolved: func(ticket string, p contract.Proposal, outcome, reason string) {
			got = append(got, resolution{ticket, p.ID, outcome, reason})
		},
	})
	dir := twoServiceTicket(t, f, "demo")
	md := "# Contract: demo\n\n## Agreed\nBase.\n\n## Proposed changes\n" +
		"### P-svc1-1 · from svc1 · open\n**What:** add a field\n**Why:** needed\n\n" +
		"### P-svc2-1 · from svc2 · open\n**What:** drop a field\n**Why:** unused\n\n## Changelog\n"
	if err := os.WriteFile(filepath.Join(dir, "contract.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, dir, &got
}

func TestContractListShowsProposalsAndAnyViolation(t *testing.T) {
	f, dir, _ := contractFixture(t)
	f.states.Update(filepath.Join(dir, "state.json"), func(s *state.State) error { s.Contract.Violation = "edited"; return nil })
	res, err := f.lc.ContractList(ctx, api.ContractListParams{Ticket: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Proposals) != 2 || res.Proposals[0].ID != "P-svc1-1" || res.Proposals[0].Status != "open" || res.Proposals[1].What != "drop a field" || res.Violation != "edited" {
		t.Errorf("list = %+v", res)
	}
	if _, err := f.lc.ContractList(ctx, api.ContractListParams{Ticket: "nope"}); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("unknown ticket = %v", err)
	}
}

func TestAcceptingWritesTheFileUpdatesStateAndTellsTheWorld(t *testing.T) {
	f, dir, got := contractFixture(t)
	f.events()
	res, err := f.lc.ContractAccept(ctx, api.ContractResolveParams{Ticket: "demo", Proposal: "P-svc1-1"})
	if err != nil || res.Outcome != "accepted" {
		t.Fatalf("accept = %+v, %v", res, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "contract.md"))
	doc := contract.Parse(string(raw))
	if p, _ := doc.Proposal("P-svc1-1"); p.Status != contract.Accepted || !strings.Contains(doc.Agreed, "add a field") {
		t.Errorf("file after accept:\n%s", raw)
	}
	st := f.state("demo")
	if !st.Contract.Pending || st.Contract.AgreedHash != doc.AgreedHash() || len(st.Contract.Resolved) != 1 {
		t.Errorf("state = %+v; one proposal is still open, and the new agreed text is the baseline", st.Contract)
	}
	var sawResolved bool
	for _, ev := range f.events() {
		sawResolved = sawResolved || ev.Type == api.EventContractResolved
	}
	if !sawResolved {
		t.Error("no contract.resolved event")
	}
	if len(*got) != 1 || (*got)[0] != (resolution{"demo", "P-svc1-1", "accepted", ""}) {
		t.Errorf("callback = %+v", *got)
	}

	f.mustContractResolve(t, false, "P-svc2-1", "unused is not a reason")
	if st := f.state("demo"); st.Contract.Pending {
		t.Error("nothing is open any more, so the pending flag must clear")
	}
}

func (f *fixture) mustContractResolve(t *testing.T, accept bool, id, reason string) {
	t.Helper()
	p := api.ContractResolveParams{Ticket: "demo", Proposal: id, Reason: reason}
	var err error
	if accept {
		_, err = f.lc.ContractAccept(ctx, p)
	} else {
		_, err = f.lc.ContractReject(ctx, p)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestRejectingNeedsAReasonAndRecordsIt(t *testing.T) {
	f, dir, got := contractFixture(t)
	if _, err := f.lc.ContractReject(ctx, api.ContractResolveParams{Ticket: "demo", Proposal: "P-svc1-1"}); apiCode(t, err) != api.CodeInvalid {
		t.Errorf("no reason = %v", err)
	}
	if len(*got) != 0 {
		t.Error("a refused rejection must not notify anyone")
	}
	f.mustContractResolve(t, false, "P-svc1-1", "too big")
	raw, _ := os.ReadFile(filepath.Join(dir, "contract.md"))
	if p, _ := contract.Parse(string(raw)).Proposal("P-svc1-1"); p.Status != contract.Rejected || p.Rejected != "too big" {
		t.Errorf("proposal = %+v", p)
	}
	if len(*got) != 1 || (*got)[0].reason != "too big" || (*got)[0].outcome != "rejected" {
		t.Errorf("callback = %+v", *got)
	}
}

func TestResolveErrorsLeaveEverythingAsItWas(t *testing.T) {
	f, _, got := contractFixture(t)
	if _, err := f.lc.ContractAccept(ctx, api.ContractResolveParams{Ticket: "demo", Proposal: "P-svc1-9"}); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("unknown = %v", err)
	}
	f.mustContractResolve(t, true, "P-svc1-1", "")
	if _, err := f.lc.ContractAccept(ctx, api.ContractResolveParams{Ticket: "demo", Proposal: "P-svc1-1"}); apiCode(t, err) != api.CodeConflict {
		t.Errorf("twice = %v", err)
	}
	if len(*got) != 1 {
		t.Errorf("callbacks = %+v; only the one real resolution counts", *got)
	}
	// a ticket without a contract
	if _, err := f.lc.ContractAccept(ctx, api.ContractResolveParams{Ticket: "none", Proposal: "P-a-1"}); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("no ticket = %v", err)
	}
}

func TestAnUnwritableContractLeavesStateUntouched(t *testing.T) {
	f, dir, got := contractFixture(t)
	before := f.state("demo")
	os.Chmod(dir, 0o500) // the atomic write cannot create its temp file
	defer os.Chmod(dir, 0o700)
	_, err := f.lc.ContractAccept(ctx, api.ContractResolveParams{Ticket: "demo", Proposal: "P-svc1-1"})
	if err == nil {
		t.Skip("directory permissions are not enforced here (running as root?)")
	}
	os.Chmod(dir, 0o700)
	after := f.state("demo")
	if len(after.Contract.Resolved) != 0 || after.Contract.AgreedHash != before.Contract.AgreedHash || len(*got) != 0 {
		t.Errorf("a failed write changed state: %+v, callbacks %+v", after.Contract, *got)
	}
}

func TestAcknowledgingClearsTheViolation(t *testing.T) {
	f, dir, _ := contractFixture(t)
	if err := f.lc.ContractAck(ctx, api.ContractAckParams{Ticket: "demo"}); apiCode(t, err) != api.CodeConflict {
		t.Errorf("nothing to ack = %v", err)
	}
	f.states.Update(filepath.Join(dir, "state.json"), func(s *state.State) error { s.Contract.Violation = "edited"; return nil })
	if err := f.lc.ContractAck(ctx, api.ContractAckParams{Ticket: "demo"}); err != nil {
		t.Fatal(err)
	}
	if v := f.state("demo").Contract.Violation; v != "" {
		t.Errorf("violation = %q", v)
	}
}

func TestProposingAppendsAnOpenProposalAndLeavesTheAgreedSectionAlone(t *testing.T) {
	f, dir, _ := contractFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "contract.md"))
	res, err := f.lc.ContractPropose(ctx, api.ContractProposeParams{Ticket: "demo", Service: "svc1", What: "add a locale", Why: "users"})
	if err != nil || res.Proposal != "P-svc1-2" {
		t.Fatalf("propose = %+v, %v", res, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "contract.md"))
	if contract.Parse(string(after)).AgreedHash() != contract.Parse(string(before)).AgreedHash() {
		t.Error("the agreed section changed")
	}
	list, _ := f.lc.ContractList(ctx, api.ContractListParams{Ticket: "demo"})
	last := list.Proposals[len(list.Proposals)-1]
	if len(list.Proposals) != 3 || last.ID != "P-svc1-2" || last.Status != "open" || last.What != "add a locale" || last.Why != "users" || len(list.Problems) != 0 {
		t.Errorf("list = %+v", list)
	}
}

func TestProposingErrors(t *testing.T) {
	f, dir, _ := contractFixture(t)
	for name, p := range map[string]api.ContractProposeParams{
		"an unknown service": {Ticket: "demo", Service: "nope", What: "w", Why: "y"},
		"no reason":          {Ticket: "demo", Service: "svc1", What: "w"},
		"structure in text":  {Ticket: "demo", Service: "svc1", What: "w\n## Agreed", Why: "y"},
	} {
		if _, err := f.lc.ContractPropose(ctx, p); apiCode(t, err) != api.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.lc.ContractPropose(ctx, api.ContractProposeParams{Ticket: "ghost", Service: "svc1", What: "w", Why: "y"}); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("unknown ticket: %v", err)
	}
	os.Remove(filepath.Join(dir, "contract.md"))
	if _, err := f.lc.ContractPropose(ctx, api.ContractProposeParams{Ticket: "demo", Service: "svc1", What: "w", Why: "y"}); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("no contract: %v", err)
	}
}
