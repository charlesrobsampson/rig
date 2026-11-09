package contract_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/contract"
)

var now = time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)

const base = `# Contract: demo

## Agreed
Dashboard API.

` + "```endpoint" + `
POST /api/v1/dashboard
` + "```" + `

## Proposed changes
### P-backend-1 · from backend · open
**What:** add ` + "`locale`" + ` to the request.
**Why:** frontend needs localized names.

### P-frontend-1 · from frontend · open
**What:** add a field:

` + "```endpoint" + `
GET /api/v1/widgets
` + "```" + `
**Why:** a new screen.

## Changelog
- 2026-11-01 P-backend-0 accepted by human: first.
`

func TestAcceptMergesTheChangeIntoAgreedUpdatesTheHeadingAndTheChangelog(t *testing.T) {
	out, err := contract.Accept(base, "P-backend-1", now)
	if err != nil {
		t.Fatal(err)
	}
	d := contract.Parse(out)
	if len(d.Problems) != 0 {
		t.Fatalf("the result does not parse cleanly: %+v\n%s", d.Problems, out)
	}
	p, _ := d.Proposal("P-backend-1")
	if p.Status != contract.Accepted {
		t.Errorf("status = %s", p.Status)
	}
	if other, _ := d.Proposal("P-frontend-1"); other.Status != contract.Open {
		t.Errorf("another proposal changed: %+v", other)
	}
	for _, want := range []string{"#### P-backend-1 (accepted 2026-11-09): add `locale` to the request.", "Dashboard API.", "POST /api/v1/dashboard"} {
		if !strings.Contains(d.Agreed, want) {
			t.Errorf("agreed lacks %q:\n%s", want, d.Agreed)
		}
	}
	if strings.Index(d.Agreed, "Dashboard API.") > strings.Index(d.Agreed, "#### P-backend-1") {
		t.Error("the accepted change must come after what was already agreed")
	}
	if len(d.Changelog) != 2 || d.Changelog[1] != "2026-11-09 P-backend-1 accepted by human: add `locale` to the request." {
		t.Errorf("changelog = %v", d.Changelog)
	}
	if !strings.HasSuffix(out, "\n") || strings.Contains(out, "\n\n\n") {
		t.Errorf("sloppy blank lines:\n%q", out)
	}
	if len(d.Proposals) != 2 || len(d.Blocks) != 1 {
		t.Errorf("proposals %d, blocks %d", len(d.Proposals), len(d.Blocks))
	}
}

func TestAcceptKeepsFencedBlocksOfTheChangeAsMachineReadableAgreedBlocks(t *testing.T) {
	out, err := contract.Accept(base, "P-frontend-1", now)
	if err != nil {
		t.Fatal(err)
	}
	d := contract.Parse(out)
	if len(d.Blocks) != 2 || d.Blocks[1].Kind != "endpoint" || !strings.Contains(d.Blocks[1].Body, "GET /api/v1/widgets") {
		t.Errorf("blocks = %+v", d.Blocks)
	}
	if len(d.Problems) != 0 {
		t.Errorf("problems = %+v", d.Problems)
	}
}

func TestRejectRecordsTheReasonUnderTheProposalAndInTheChangelog(t *testing.T) {
	out, err := contract.Reject(base, "P-backend-1", "too   disruptive\nfor this release", now)
	if err != nil {
		t.Fatal(err)
	}
	d := contract.Parse(out)
	p, _ := d.Proposal("P-backend-1")
	if len(d.Problems) != 0 || p.Status != contract.Rejected || p.Rejected != "too disruptive for this release" {
		t.Fatalf("proposal = %+v, problems %+v\n%s", p, d.Problems, out)
	}
	if !strings.Contains(out, "**Why:** frontend needs localized names.\n**Rejected:** too disruptive for this release\n") {
		t.Errorf("the reason should sit directly under the proposal's text:\n%s", out)
	}
	if d.Changelog[len(d.Changelog)-1] != "2026-11-09 P-backend-1 rejected by human: too disruptive for this release" {
		t.Errorf("changelog = %v", d.Changelog)
	}
	if strings.Contains(d.Agreed, "locale") {
		t.Error("a rejected change must not reach the agreed section")
	}
	if other, _ := d.Proposal("P-frontend-1"); other.Status != contract.Open {
		t.Errorf("another proposal changed: %+v", other)
	}
}

func TestEditErrors(t *testing.T) {
	if _, err := contract.Accept(base, "P-backend-9", now); !errors.Is(err, contract.ErrNoProposal) {
		t.Errorf("an unknown id = %v", err)
	}
	accepted, _ := contract.Accept(base, "P-backend-1", now)
	if _, err := contract.Accept(accepted, "P-backend-1", now); !errors.Is(err, contract.ErrNotOpen) || !strings.Contains(err.Error(), "already accepted") {
		t.Errorf("accepting twice = %v", err)
	}
	if _, err := contract.Reject(accepted, "P-backend-1", "no", now); !errors.Is(err, contract.ErrNotOpen) {
		t.Errorf("rejecting an accepted one = %v", err)
	}
	if _, err := contract.Reject(base, "P-backend-1", "   ", now); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Errorf("a rejection without a reason = %v", err)
	}
	if _, err := contract.Accept("# C\n\n## Proposed changes\n### P-a-1 · from a · open\n**What:** w\n**Why:** y\n", "P-a-1", now); err == nil || !strings.Contains(err.Error(), "## Agreed") {
		t.Errorf("a contract without an agreed section = %v", err)
	}
}

func TestEditsWorkOnMinimalAndAwkwardFiles(t *testing.T) {
	// no changelog section yet
	noLog := "# C\n\n## Agreed\n\n## Proposed changes\n### P-a-1 · from a · open\n**What:** w\n**Why:** y\n"
	out, err := contract.Accept(noLog, "P-a-1", now)
	if err != nil {
		t.Fatal(err)
	}
	d := contract.Parse(out)
	if len(d.Problems) != 0 || len(d.Changelog) != 1 || !strings.Contains(d.Agreed, "#### P-a-1") {
		t.Errorf("without a changelog: %+v\n%s", d, out)
	}

	// the template's comments survive an accept
	tmpl := contract.Template("demo") + "\n"
	tmpl = strings.Replace(tmpl, "## Changelog", "### P-api-1 · from api · open\n**What:** hello\n**Why:** y\n\n## Changelog", 1)
	out, err = contract.Accept(tmpl, "P-api-1", now)
	if err != nil || !strings.Contains(out, "<!-- What the services have agreed on") || !strings.Contains(out, "<!-- Agents append entries") {
		t.Errorf("the guidance comments were lost or the accept failed: %v\n%s", err, out)
	}
	if d := contract.Parse(out); len(d.Problems) != 0 || d.AgreedEmpty() {
		t.Errorf("after accepting into a template: %+v", d)
	}

	// CRLF input is normalized, and the last proposal in the file works
	crlf := strings.ReplaceAll(noLog, "\n", "\r\n")
	if _, err := contract.Reject(crlf, "P-a-1", "no", now); err != nil {
		t.Errorf("CRLF: %v", err)
	}
}

func TestTheAgreedHashIsStableAcrossAcceptedAndRejectedButNotAcrossAnAccept(t *testing.T) {
	before := contract.Parse(base).AgreedHash()
	rejected, _ := contract.Reject(base, "P-backend-1", "no", now)
	accepted, _ := contract.Accept(base, "P-backend-1", now)
	if contract.Parse(rejected).AgreedHash() != before {
		t.Error("a rejection must not change the agreed hash")
	}
	if contract.Parse(accepted).AgreedHash() == before {
		t.Error("an accept changes the agreed section, so the hash must change")
	}
}
