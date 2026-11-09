package contract_test

import (
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/contract"
)

const sample = "# Contract: ticket-412\n" + `
## Agreed
Free markdown.

` + "```endpoint" + `
POST /api/v1/dashboard
request: {"user_id": "string"}
` + "```" + `

` + "```schema" + `
Widget: {id: string}
` + "```" + `

` + "```sh" + `
echo not machine readable
` + "```" + `

## Proposed changes
### P-backend-1 · from backend · open
**What:** add ` + "`locale`" + ` to the request.
**Why:** frontend needs localized widget names.

### P-frontend-1 · from frontend · accepted
**What:** drop the legacy field.
**Why:** unused.

### P-frontend-2 · from frontend · rejected
**What:** rename everything.
**Why:** consistency.
**Rejected:** too disruptive for this release.

## Changelog
- 2026-11-05 P-frontend-1 accepted by human: dropped the legacy field.
- 2026-11-06 P-frontend-2 rejected by human: too disruptive.
`

func TestParseReadsEveryPartOfAContract(t *testing.T) {
	d := contract.Parse(sample)
	if len(d.Problems) != 0 {
		t.Fatalf("problems = %+v", d.Problems)
	}
	if d.Title != "Contract: ticket-412" || !strings.Contains(d.Agreed, "Free markdown.") || d.AgreedEmpty() {
		t.Errorf("title %q, agreed %q", d.Title, d.Agreed)
	}
	if len(d.Blocks) != 2 || d.Blocks[0].Kind != "endpoint" || !strings.Contains(d.Blocks[0].Body, "POST /api/v1/dashboard") || d.Blocks[1].Kind != "schema" || d.Blocks[0].Line != 6 {
		t.Errorf("blocks = %+v", d.Blocks)
	}
	if len(d.Proposals) != 3 {
		t.Fatalf("proposals = %+v", d.Proposals)
	}
	first := d.Proposals[0]
	if first.ID != "P-backend-1" || first.From != "backend" || first.Status != contract.Open || first.What != "add `locale` to the request." || first.Why != "frontend needs localized widget names." || first.Line != 20 {
		t.Errorf("first = %+v", first)
	}
	if last := d.Proposals[2]; last.Status != contract.Rejected || last.Rejected != "too disruptive for this release." {
		t.Errorf("last = %+v", last)
	}
	if open := d.OpenProposals(); len(open) != 1 || open[0].ID != "P-backend-1" {
		t.Errorf("open = %+v", open)
	}
	if p, ok := d.Proposal("P-frontend-1"); !ok || p.Status != contract.Accepted {
		t.Errorf("Proposal lookup = %+v, %v", p, ok)
	}
	if len(d.Changelog) != 2 || !strings.HasPrefix(d.Changelog[0], "2026-11-05 P-frontend-1 accepted") {
		t.Errorf("changelog = %v", d.Changelog)
	}
}

func TestZeroOneAndManyProposals(t *testing.T) {
	for n, count := range map[string]int{"none": 0, "one": 1, "many": 5} {
		var b strings.Builder
		b.WriteString("# C\n\n## Agreed\nx\n\n## Proposed changes\n")
		for i := 1; i <= count; i++ {
			b.WriteString("### P-api-" + string(rune('0'+i)) + " · from api · open\n**What:** w\n**Why:** y\n\n")
		}
		b.WriteString("## Changelog\n")
		d := contract.Parse(b.String())
		if len(d.Proposals) != count || len(d.Problems) != 0 {
			t.Errorf("%s: %d proposals, problems %+v", n, len(d.Proposals), d.Problems)
		}
	}
}

func TestAMalformedEntryIsReportedWithoutHidingTheValidOnes(t *testing.T) {
	md := strings.Join([]string{
		"# C", "", "## Agreed", "x", "", "## Proposed changes", // 6
		"### P-api-1 · from api · open", "**What:** fine", "**Why:** fine", "", // 7-10
		"### a proposal without the format", "**What:** w", "**Why:** y", "", // 11-14
		"### P-api-2 · from web · open", "**What:** author mismatch", "**Why:** y", "", // 15-18
		"### P-api-3 · from api · open", "**What:** no why", "", // 19-21
		"### P-api-1 · from api · open", "**What:** a repeat", "**Why:** y", "", // 22-25
		"### P-api-4 · from api · rejected", "**What:** w", "**Why:** y", "", // 26-29
		"### P-api-5 · from api · open", "**What:** also fine", "**Why:** fine", "", // 30-33
		"## Changelog",
	}, "\n")
	d := contract.Parse(md)
	var ids []string
	for _, p := range d.Proposals {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "P-api-1,P-api-5" {
		t.Errorf("valid proposals = %v, want P-api-1 and P-api-5", ids)
	}
	problems := map[int]string{}
	for _, p := range d.Problems {
		problems[p.Line] += p.Message + "|"
	}
	for line, want := range map[int]string{
		11: "not a proposal heading",
		15: `names service "api" but the proposal says it is from "web"`,
		19: "no **Why:** line",
		22: "already used on line 7",
		26: "needs a **Rejected:** reason",
	} {
		if !strings.Contains(problems[line], want) {
			t.Errorf("line %d: problems %q lack %q", line, problems[line], want)
		}
	}
	if len(d.Problems) != 5 {
		t.Errorf("%d problems: %+v", len(d.Problems), d.Problems)
	}
}

func TestMultiLineFieldsAndCodeFencesInsideProposals(t *testing.T) {
	md := "# C\n## Agreed\nx\n## Proposed changes\n### P-api-1 · from api · open\n**What:** add a field\nto the response:\n\n```endpoint\n### not-a-heading inside a fence\nGET /x\n```\n**Why:** the web needs it\nand soon.\n## Changelog\n"
	d := contract.Parse(md)
	if len(d.Problems) != 0 || len(d.Proposals) != 1 {
		t.Fatalf("problems %+v, proposals %+v", d.Problems, d.Proposals)
	}
	p := d.Proposals[0]
	if !strings.HasPrefix(p.What, "add a field\nto the response:") || !strings.Contains(p.What, "GET /x") || p.Why != "the web needs it\nand soon." {
		t.Errorf("proposal = %+v", p)
	}
}

func TestSectionsAreReportedWhenMissingOrRepeated(t *testing.T) {
	d := contract.Parse("# C\n\n## Agreed\nx\n")
	msgs := ""
	for _, p := range d.Problems {
		msgs += p.Message + "\n"
	}
	if !strings.Contains(msgs, `"## Proposed changes" is missing`) || !strings.Contains(msgs, `"## Changelog" is missing`) || strings.Contains(msgs, `"## Agreed" is missing`) {
		t.Errorf("problems:\n%s", msgs)
	}
	dup := contract.Parse("# C\n## Agreed\nfirst\n## Agreed\nsecond\n## Proposed changes\n## Changelog\n")
	if dup.Agreed != "first" || len(dup.Problems) != 1 || !strings.Contains(dup.Problems[0].Message, "appears twice") {
		t.Errorf("a repeated section: agreed %q, problems %+v", dup.Agreed, dup.Problems)
	}
	empty := contract.Parse("")
	if len(empty.Proposals) != 0 || len(empty.Problems) != 3 {
		t.Errorf("an empty file: %+v", empty)
	}
}

func TestTheTemplateParsesCleanlyAndHasNothingAgreedYet(t *testing.T) {
	md := contract.Template("ticket-412")
	d := contract.Parse(md)
	if d.Title != "Contract: ticket-412" || len(d.Problems) != 0 || len(d.Proposals) != 0 || len(d.Blocks) != 0 || !d.AgreedEmpty() || len(d.Changelog) != 0 {
		t.Errorf("template parsed as %+v", d)
	}
	for _, want := range []string{"## Agreed", "## Proposed changes", "## Changelog", "### P-<service>-<n> · from <service> · open"} {
		if !strings.Contains(md, want) {
			t.Errorf("the template lacks %q", want)
		}
	}
	if d.Agreed != "" {
		t.Errorf("the guidance comment leaked into the agreed text: %q", d.Agreed)
	}
	// real content next to the comment makes it non-empty
	filled := strings.Replace(md, "-->\n\n## Proposed", "-->\n\n```endpoint\nGET /x\n```\n\n## Proposed", 1)
	if got := contract.Parse(filled); got.AgreedEmpty() || len(got.Blocks) != 1 || got.Blocks[0].Line == 0 {
		t.Errorf("filled = %+v", got)
	}
}

func TestLineEndingsAndCommentsDoNotShiftLineNumbers(t *testing.T) {
	md := "# C\r\n\r\n## Agreed\r\n<!-- a\r\nmulti-line\r\ncomment -->\r\nreal text\r\n\r\n## Proposed changes\r\n### P-api-1 · from api · open\r\n**What:** w\r\n**Why:** y\r\n## Changelog\r\n"
	d := contract.Parse(md)
	if d.Agreed != "real text" || len(d.Proposals) != 1 || d.Proposals[0].Line != 10 || len(d.Problems) != 0 {
		t.Errorf("parsed = %+v", d)
	}
}

func TestNextProposalIDIsStablePerService(t *testing.T) {
	md := contract.Template("t") + "\n### P-api-1 · from api · open\n**What:** w\n**Why:** y\n### P-web-7 · from web · accepted\n### P-api-3 · from api · open\n"
	tests := map[string]string{"api": "P-api-4", "web": "P-web-8", "worker": "P-worker-1"}
	for service, want := range tests {
		if got := contract.NextProposalID(md, service); got != want {
			t.Errorf("NextProposalID(%s) = %s, want %s", service, got, want)
		}
	}
	if contract.NextProposalID("", "api") != "P-api-1" {
		t.Error("an empty file starts at 1")
	}
}
