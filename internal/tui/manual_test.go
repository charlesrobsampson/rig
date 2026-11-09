package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func manualResult(signed bool) *api.TestsRenderResult {
	return &api.TestsRenderResult{
		Preamble:  "Check demo by hand.",
		SignedOff: signed,
		Steps: []api.TestsStep{
			{ID: "health", Title: "health responds", Body: "Open it:\n\n```sh\ncurl http://localhost:41000/healthz\n```\n\nand then\n\n```sh\ncurl http://localhost:41000/ready\n```", Done: true},
			{ID: "login", Title: "log in", Body: "Use the form."},
			{ID: "logout", Title: "log out", Body: "```\ncurl -X POST http://localhost:41000/logout\n```"},
		},
	}
}

// manualOpen opens the demo ticket on its manual tab.
func manualOpen(t *testing.T, src *fakeSource) Model {
	t.Helper()
	m := ready(t, src, api.TicketSummary{ID: "demo", Phase: domain.PhaseTest, Services: []api.ServiceSummary{{Name: "api"}}})
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return press(t, m, "enter", "6")
}

func TestTheManualTabShowsStepsWithTheirState(t *testing.T) {
	src := &fakeSource{manual: manualResult(false)}
	m := manualOpen(t, src)
	v := m.View()
	for _, want := range []string{"[manual 1/3]", "1/3 steps done · not signed off", "Check demo by hand.", "> [x] health: health responds", "curl http://localhost:41000/healthz", "  [ ] login: log in", "  [ ] logout: log out"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Use the form.") {
		t.Errorf("only the selected step is expanded:\n%s", v)
	}
	m = press(t, m, "j")
	if v := m.View(); !strings.Contains(v, "> [ ] login: log in") || !strings.Contains(v, "Use the form.") || strings.Contains(v, "curl http://localhost:41000/healthz") {
		t.Errorf("after j:\n%s", v)
	}
}

func TestASignedOffTicketSaysSo(t *testing.T) {
	m := manualOpen(t, &fakeSource{manual: manualResult(true)})
	if v := m.View(); !strings.Contains(v, "SIGNED OFF · 1/3 steps done") {
		t.Errorf("view:\n%s", v)
	}
}

func TestSpaceTicksAndUntickssTheSelectedStepAndTheViewFollows(t *testing.T) {
	src := &fakeSource{manual: manualResult(false)}
	m := manualOpen(t, src)
	m = press(t, m, "j", "space")
	if strings.Join(src.acts, "|") != "step demo login done=true" {
		t.Fatalf("acts = %v", src.acts)
	}
	if v := m.View(); !strings.Contains(v, "> [x] login: log in") || !strings.Contains(v, "[manual 2/3]") || !strings.Contains(v, "step login ticked") {
		t.Errorf("after ticking:\n%s", v)
	}
	m = press(t, m, "enter") // enter does the same
	if src.acts[len(src.acts)-1] != "step demo login done=false" || !strings.Contains(m.View(), "step login unticked") {
		t.Errorf("acts = %v\n%s", src.acts, m.View())
	}
	if !strings.Contains(m.View(), "> [ ] login: log in") {
		t.Errorf("the selection moved away from the step:\n%s", m.View())
	}
}

func TestSelectionStaysInsideTheStepList(t *testing.T) {
	m := manualOpen(t, &fakeSource{manual: manualResult(false)})
	m = press(t, m, "k", "k")
	if m.det.step != 0 {
		t.Errorf("step = %d above the top", m.det.step)
	}
	m = press(t, m, "j", "j", "j", "j", "j")
	if m.det.step != 2 {
		t.Errorf("step = %d below the bottom", m.det.step)
	}
	m = press(t, m, "g")
	if m.det.step != 0 {
		t.Errorf("g: step = %d", m.det.step)
	}
	m = press(t, m, "G")
	if m.det.step != 2 {
		t.Errorf("G: step = %d", m.det.step)
	}
}

func TestYCopiesTheCodeBlocksOfTheSelectedStep(t *testing.T) {
	src := &fakeSource{manual: manualResult(false)}
	m := manualOpen(t, src)
	var copied []string
	m.opts.Copy = func(s string) error { copied = append(copied, s); return nil }
	m = press(t, m, "y")
	if len(copied) != 1 || copied[0] != "curl http://localhost:41000/healthz\n\ncurl http://localhost:41000/ready" {
		t.Errorf("copied %q", copied)
	}
	if !strings.Contains(statusLine(m), "copied 2 code blocks of step health") {
		t.Errorf("status = %q", statusLine(m))
	}
	m = press(t, m, "j", "y") // login has no code block
	if len(copied) != 1 || !strings.Contains(statusLine(m), "step login has no code block to copy") {
		t.Errorf("copied %q, status %q", copied, statusLine(m))
	}
	m.opts.Copy = func(string) error { return errors.New("no clipboard") }
	m = press(t, m, "j", "y")
	if !strings.Contains(statusLine(m), "error: cannot copy: no clipboard") {
		t.Errorf("status = %q", statusLine(m))
	}
}

func TestCodeBlocksExtraction(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"none", "just text", nil},
		{"one", "text\n```sh\na\nb\n```\nmore", []string{"a\nb"}},
		{"two with languages", "```sh\na\n```\n~~~\nb\n~~~", []string{"a", "b"}},
		{"indented fences", "  ```\n  a\n  ```", []string{"  a"}},
		{"unclosed", "```\na\nb", []string{"a\nb"}},
		{"empty unclosed", "```\n", nil},
		{"empty block", "```\n```", []string{""}},
	}
	for _, tt := range tests {
		got := CodeBlocks(tt.body)
		if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") || len(got) != len(tt.want) {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSignOffAsksThenSignsOff(t *testing.T) {
	src := &fakeSource{manual: manualResult(false)}
	m := manualOpen(t, src)
	m = press(t, m, "S")
	if !strings.Contains(statusLine(m), "Sign off the manual tests of demo? [y/N]") || len(src.acts) != 0 {
		t.Fatalf("status %q, acts %v", statusLine(m), src.acts)
	}
	m = press(t, m, "y")
	if strings.Join(src.acts, "|") != "signoff demo override=false" || !strings.Contains(statusLine(m), "manual tests of demo signed off") {
		t.Errorf("acts %v, status %q", src.acts, statusLine(m))
	}
}

func TestSignOffWithStepsNotDoneOffersAnOverride(t *testing.T) {
	blocked := &api.Error{Code: api.CodeBlocked, Message: "2 of 3 manual-test steps are not done", Details: []string{"step not done: login", "step not done: logout"}}
	src := &fakeSource{manual: manualResult(false), signoffErr: blocked}
	m := manualOpen(t, src)
	m = press(t, m, "S", "y")
	v := statusLine(m)
	if !strings.Contains(v, "2 of 3 manual-test steps are not done") || !strings.Contains(v, "step not done: login") || !strings.Contains(v, "Sign off anyway? [y/N]") {
		t.Fatalf("status = %q", v)
	}
	if strings.Join(src.acts, "|") != "signoff demo override=false" {
		t.Fatalf("acts = %v", src.acts)
	}
	declined := press(t, m, "n")
	if len(src.acts) != 1 || declined.modal != nil {
		t.Errorf("declining signed off anyway: %v", src.acts)
	}
	m = press(t, m, "y")
	if strings.Join(src.acts, "|") != "signoff demo override=false|signoff demo override=true" {
		t.Errorf("acts = %v", src.acts)
	}
}

func TestSignOffAnswersAnythingElseWithTheError(t *testing.T) {
	src := &fakeSource{manual: manualResult(false), signoffErr: &api.Error{Code: api.CodeConflict, Message: "ticket is in plan"}}
	m := manualOpen(t, src)
	m = press(t, m, "S", "y")
	if v := statusLine(m); !strings.Contains(v, "error: ticket is in plan") || m.modal != nil {
		t.Errorf("status = %q", v)
	}
}

func TestManualTabProblemsAreExplained(t *testing.T) {
	m := manualOpen(t, &fakeSource{})
	if v := m.View(); !strings.Contains(v, "No manual-tests.md yet") {
		t.Errorf("missing file:\n%s", v)
	}
	m = press(t, m, "space", "y") // nothing to tick or copy, and nothing may panic
	m = manualOpen(t, &fakeSource{manual: &api.TestsRenderResult{}})
	if v := m.View(); !strings.Contains(v, "No steps yet") {
		t.Errorf("no steps:\n%s", v)
	}
	m = press(t, m, "space")
	if !strings.Contains(statusLine(m), "there are no steps to tick") {
		t.Errorf("status = %q", statusLine(m))
	}
	m = press(t, m, "y")
	if !strings.Contains(statusLine(m), "no steps to copy from") {
		t.Errorf("status = %q", statusLine(m))
	}
	bad := &fakeSource{manual: manualResult(false), actErr: map[string]error{"manual": errors.New("manual-tests.md cannot be rendered: unknown placeholder {{x.url}}")}}
	m = manualOpen(t, bad)
	if v := m.View(); !strings.Contains(v, "Could not load manual") || !strings.Contains(v, "unknown placeholder") {
		t.Errorf("render error:\n%s", v)
	}
}

func TestAStepErrorChangesNothing(t *testing.T) {
	src := &fakeSource{manual: manualResult(false), actErr: map[string]error{"step": &api.Error{Code: api.CodeNotFound, Message: `manual-tests.md has no step "health"`}}}
	m := manualOpen(t, src)
	m = press(t, m, "space")
	if v := m.View(); !strings.Contains(v, "error: manual-tests.md has no step") || !strings.Contains(v, "> [x] health") {
		t.Errorf("view:\n%s", v)
	}
}

func TestALongStepListScrollsToKeepTheSelectionVisible(t *testing.T) {
	r := &api.TestsRenderResult{}
	for i := 0; i < 30; i++ {
		id := "s" + string(rune('a'+i/10)) + string(rune('0'+i%10))
		r.Steps = append(r.Steps, api.TestsStep{ID: id, Title: "step " + id, Body: "body of " + id})
	}
	m := manualOpen(t, &fakeSource{manual: r})
	m, _ = send(m, tea.WindowSizeMsg{Width: 80, Height: 12})
	for range 25 {
		m = press(t, m, "j")
	}
	v := m.View()
	if !strings.Contains(v, "> [ ] sc5: step sc5") || strings.Contains(v, "sa0:") {
		t.Errorf("view:\n%s", v)
	}
	if n := len(strings.Split(v, "\n")); n > 12 {
		t.Errorf("%d lines on a 12-line screen", n)
	}
	m = press(t, m, "g")
	if v := m.View(); !strings.Contains(v, "> [ ] sa0") {
		t.Errorf("after g:\n%s", v)
	}
}
