package manualtests_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/manualtests"
)

func vars() manualtests.Vars {
	return manualtests.Vars{
		TicketID: "proj-412",
		Services: map[string]manualtests.ServiceVars{
			"api":    {HostPort: 41000, InternalPort: 8080},
			"web":    {HostPort: 41001, InternalPort: 3000},
			"worker": {NotRunning: "stopped", InternalPort: 9000},
			"cli":    {HostPort: 41002}, // publishes a port but has no internal_port
		},
		Dependencies: map[string]bool{"db": true},
	}
}

func TestParseSplitsTheFileIntoSteps(t *testing.T) {
	doc, err := manualtests.Parse(strings.Join([]string{
		"# Manual tests", "", "Intro line.", "",
		"### login-works: the login page responds", "", "Open the page.", "```sh", "curl localhost/", "### not-a-step: inside a fence", "```", "",
		"### api-1: a: title with a colon", "Second body.",
		"### last-one:   spaced title  ", "",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Preamble != "# Manual tests\n\nIntro line." {
		t.Errorf("preamble = %q", doc.Preamble)
	}
	want := []manualtests.Step{
		{ID: "login-works", Title: "the login page responds", Body: "Open the page.\n```sh\ncurl localhost/\n### not-a-step: inside a fence\n```"},
		{ID: "api-1", Title: "a: title with a colon", Body: "Second body."},
		{ID: "last-one", Title: "spaced title", Body: ""},
	}
	if !reflect.DeepEqual(doc.Steps, want) {
		t.Errorf("steps = %+v\nwant   %+v", doc.Steps, want)
	}
}

func TestParseOfAnEmptyOrStepLessFile(t *testing.T) {
	for _, md := range []string{"", "just some text\n", "# Title only\n"} {
		doc, err := manualtests.Parse(md)
		if err != nil || len(doc.Steps) != 0 {
			t.Errorf("Parse(%q) = %+v, %v", md, doc, err)
		}
	}
}

func TestParseReportsEveryBadHeadingWithItsLine(t *testing.T) {
	md := strings.Join([]string{
		"### ok-one: fine",       // 1
		"### no colon here",      // 2
		"### Bad_Id: capitals",   // 3
		"### ok-one: a repeat",   // 4
		"### ok-two: fine again", // 5
		"###",                    // 6
	}, "\n")
	_, err := manualtests.Parse(md)
	var problems *manualtests.Problems
	if !errors.As(err, &problems) {
		t.Fatalf("error = %v", err)
	}
	joined := strings.Join(problems.Problems, "\n")
	for _, want := range []string{"line 2:", "not a step heading", "line 3:", `"Bad_Id"`, "lowercase", "line 4:", "already used on line 1", "line 6:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q:\n%s", want, joined)
		}
	}
	if len(problems.Problems) != 4 {
		t.Errorf("%d problems, want 4:\n%s", len(problems.Problems), joined)
	}
}

func TestRenderFillsEveryKindOfPlaceholder(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"host url", "{{api.url}}/healthz", "http://localhost:41000/healthz"},
		{"host port", "port {{api.port}}", "port 41000"},
		{"internal url", "{{api.internal_url}}", "http://api:8080"},
		{"dependency host", "psql -h {{db.host}}", "psql -h db"},
		{"ticket id", "ticket {{ticket.id}}", "ticket proj-412"},
		{"spaces inside the braces", "{{ api.url }} and {{  web.port  }}", "http://localhost:41000 and 41001"},
		{"several services", "{{api.port}} {{web.port}}", "41000 41001"},
		{"the same one repeated", "{{api.port}}-{{api.port}}", "41000-41000"},
		{"no placeholders at all", "plain text, {single} braces and $VAR and ${{ not closed", "plain text, {single} braces and $VAR and ${{ not closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := manualtests.Render("### s: t\n\n"+tt.in+"\n", vars())
			if err != nil || len(doc.Steps) != 1 || doc.Steps[0].Body != tt.want {
				t.Errorf("Render = %+v, %v; want body %q", doc, err, tt.want)
			}
		})
	}
}

func TestRenderFillsTitlesPreamblesAndCodeFences(t *testing.T) {
	md := "Intro for {{ticket.id}}\n\n### check-{{x}}: ignored\n"
	if _, err := manualtests.Render(md, vars()); err == nil {
		t.Error("a bad step id containing a placeholder should still be a problem")
	}
	doc, err := manualtests.Render("Intro for {{ticket.id}}\n\n### hit: GET {{api.url}}\n\n```sh\ncurl {{api.url}}/x\n```\n", vars())
	if err != nil || doc.Preamble != "Intro for proj-412" || doc.Steps[0].Title != "GET http://localhost:41000" || !strings.Contains(doc.Steps[0].Body, "curl http://localhost:41000/x") {
		t.Errorf("Render = %+v, %v", doc, err)
	}
}

func TestRenderReportsEveryUnfillablePlaceholderAtOnce(t *testing.T) {
	md := strings.Join([]string{
		"### one: a", "{{api.url}} {{nope.url}} {{api.bogus}} {{db.port}} {{whatever}} {{worker.url}} {{cli.internal_url}}",
		"### two: b", "{{nope.url}} {{ticket.nope}} {{web.port}}",
	}, "\n")
	_, err := manualtests.Render(md, vars())
	var problems *manualtests.Problems
	if !errors.As(err, &problems) {
		t.Fatalf("error = %v", err)
	}
	joined := strings.Join(problems.Problems, "\n")
	for _, want := range []string{
		`step one: {{nope.url}}: "nope" is neither a service nor a dependency`,
		`step one: {{api.bogus}}: service api has no "bogus"`,
		`step one: {{db.port}}: dependency db has no "port" (try host)`,
		"step one: {{whatever}}: not a placeholder",
		"step one: {{worker.url}}: service worker has no host port right now (stopped); start the ticket first",
		"step one: {{cli.internal_url}}: service cli has no internal_port",
		`step two: {{nope.url}}`,
		`step two: {{ticket.nope}}`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q:\n%s", want, joined)
		}
	}
	if len(problems.Problems) != 8 {
		t.Errorf("%d problems, want 8 (every offender, and a fine placeholder is not one):\n%s", len(problems.Problems), joined)
	}
	if strings.Contains(joined, "{{api.url}}") || strings.Contains(joined, "{{web.port}}") {
		t.Error("a placeholder that resolves was reported")
	}
}

func TestAStoppedServiceGivesAClearMessage(t *testing.T) {
	_, err := manualtests.Render("### s: t\ncurl {{worker.url}}\n", vars())
	if err == nil || !strings.Contains(err.Error(), "service worker has no host port right now (stopped)") || !strings.Contains(err.Error(), "start the ticket first") {
		t.Errorf("error = %v", err)
	}
	v := vars()
	v.Services["worker"] = manualtests.ServiceVars{}
	if _, err := manualtests.Render("### s: t\n{{worker.port}}\n", v); err == nil || !strings.Contains(err.Error(), "it publishes none") {
		t.Errorf("a service that never had a port: %v", err)
	}
}
