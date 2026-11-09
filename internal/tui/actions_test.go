package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

var (
	oneSvc = api.TicketSummary{ID: "demo", Title: "Demo", Phase: domain.PhasePlan, PendingGate: domain.GatePlanApproval,
		Services: []api.ServiceSummary{{Name: "api"}}}
	twoSvc = api.TicketSummary{ID: "multi", Title: "Multi", Phase: domain.PhaseImplement,
		Services: []api.ServiceSummary{{Name: "api"}, {Name: "web"}}}
)

// ready is a connected model showing the given fleet on a roomy screen.
func ready(t *testing.T, src *fakeSource, tickets ...api.TicketSummary) Model {
	t.Helper()
	src.tickets = tickets
	m := New(Options{Source: src, RefreshEvery: 1})
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 20})
	m, _ = load(t, m)
	return m
}

// press sends keys one by one, running every command they produce.
func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		var cmd tea.Cmd
		switch k {
		case "enter":
			m, cmd = send(m, tea.KeyMsg{Type: tea.KeyEnter})
		case "esc":
			m, cmd = send(m, tea.KeyMsg{Type: tea.KeyEsc})
		case "backspace":
			m, cmd = send(m, tea.KeyMsg{Type: tea.KeyBackspace})
		case "space":
			m, cmd = send(m, tea.KeyMsg{Type: tea.KeySpace})
		default:
			m, cmd = send(m, key(k))
		}
		m, _ = run(t, m, cmd)
	}
	return m
}

func typeText(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		if r == ' ' {
			m = press(t, m, "space")
		} else {
			m = press(t, m, string(r))
		}
	}
	return m
}

func statusLine(m Model) string {
	lines := strings.Split(m.View(), "\n")
	return lines[len(lines)-1]
}

func TestApproveAsksThenApprovesThePendingGate(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, oneSvc)
	m = press(t, m, "a")
	if !strings.Contains(statusLine(m), "Approve plan_approval for demo? [y/N]") {
		t.Fatalf("no confirmation: %q", statusLine(m))
	}
	if len(src.acts) != 0 {
		t.Fatalf("acted before the answer: %v", src.acts)
	}
	m = press(t, m, "y")
	if strings.Join(src.acts, "|") != "approve demo plan_approval" {
		t.Errorf("acts = %v", src.acts)
	}
	if !strings.Contains(statusLine(m), "plan_approval approved for demo") {
		t.Errorf("status = %q", statusLine(m))
	}
}

func TestNothingToApproveSaysSoWithoutCallingTheDaemon(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, twoSvc)
	m = press(t, m, "a")
	if len(src.acts) != 0 || !strings.Contains(statusLine(m), "error: multi: nothing waits for your approval") {
		t.Errorf("acts %v, status %q", src.acts, statusLine(m))
	}
}

func TestEveryDestructiveActionNeedsAYesAndAnythingElseCancels(t *testing.T) {
	for _, k := range []string{"a", "v", "p", "x"} {
		for _, answer := range []string{"n", "N", "esc", "enter", "q", "z"} {
			src := &fakeSource{}
			m := ready(t, src, oneSvc)
			m = press(t, m, k)
			if m.modal == nil {
				t.Fatalf("%s: no question asked", k)
			}
			m = press(t, m, answer)
			if len(src.acts) != 0 {
				t.Errorf("%s then %q acted anyway: %v", k, answer, src.acts)
			}
			if m.modal != nil || !strings.Contains(statusLine(m), "cancelled") {
				t.Errorf("%s then %q: modal %v, status %q", k, answer, m.modal != nil, statusLine(m))
			}
		}
	}
}

func TestEachActionKeyMakesTheRightCall(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want string
		note string
	}{
		{"review", []string{"v", "y"}, "phase demo review", "demo is in review"},
		{"push", []string{"p", "y"}, "push demo", "demo pushed and done"},
		{"stop", []string{"x", "y"}, "stop demo force=false", "demo stopped"},
	}
	for _, tt := range tests {
		src := &fakeSource{pushRes: api.PushRunResult{Done: true, Services: []api.PushServiceResult{{Service: "api", Pushed: true}}}}
		m := ready(t, src, oneSvc)
		m = press(t, m, tt.keys...)
		if strings.Join(src.acts, "|") != tt.want || !strings.Contains(statusLine(m), tt.note) {
			t.Errorf("%s: acts %v, status %q; want %q and %q", tt.name, src.acts, statusLine(m), tt.want, tt.note)
		}
	}
}

func TestActionsApplyToTheSelectedRowAndToTheOpenTicket(t *testing.T) {
	src := &fakeSource{}
	other := api.TicketSummary{ID: "aaa", PendingGate: domain.GateReviewApproval, Services: []api.ServiceSummary{{Name: "api"}}}
	m := ready(t, src, oneSvc, other) // both need a gate: sorted by id, aaa first
	m = press(t, m, "j", "a", "y")    // second row: demo
	m = press(t, m, "g", "enter", "a", "y")
	if strings.Join(src.acts, "|") != "approve demo plan_approval|approve aaa review_approval" {
		t.Errorf("acts = %v", src.acts)
	}
}

func TestAnErrorFromTheDaemonShowsInTheStatusBarAndChangesNothing(t *testing.T) {
	src := &fakeSource{actErr: map[string]error{"approve": &api.Error{Code: api.CodeBlocked, Message: "cannot approve the plan", Details: []string{"contract: write the agreed section"}}}}
	m := ready(t, src, oneSvc)
	before := m.dash.selected
	m = press(t, m, "a", "y")
	if v := statusLine(m); !strings.Contains(v, "error: cannot approve the plan — contract: write the agreed section") {
		t.Errorf("status = %q", v)
	}
	if m.modal != nil || m.dash.selected != before || m.det != nil || !m.connected {
		t.Error("a failed action changed the screen")
	}
	// the next key clears the message
	m = press(t, m, "j")
	if strings.Contains(statusLine(m), "error:") {
		t.Errorf("the error stuck: %q", statusLine(m))
	}
}

func TestAStopRefusedForUnsavedWorkOffersToForce(t *testing.T) {
	blocked := &api.Error{Code: api.CodeBlocked, Message: "work would be lost", Details: []string{"api: uncommitted changes"}}
	src := &fakeSource{stopErr: blocked}
	m := ready(t, src, oneSvc)
	m = press(t, m, "x", "y")
	if v := statusLine(m); !strings.Contains(v, "api: uncommitted changes") || !strings.Contains(v, "Force it and discard that work? [y/N]") {
		t.Fatalf("status = %q", v)
	}
	if strings.Join(src.acts, "|") != "stop demo force=false" {
		t.Fatalf("acts = %v", src.acts)
	}
	// no: nothing is forced
	cancelled := press(t, m, "n")
	if strings.Join(src.acts, "|") != "stop demo force=false" || cancelled.modal != nil {
		t.Errorf("declining forced anyway: %v", src.acts)
	}
	// yes: the second stop is forced
	m = press(t, m, "y")
	if strings.Join(src.acts, "|") != "stop demo force=false|stop demo force=true" || !strings.Contains(statusLine(m), "demo stopped") {
		t.Errorf("acts = %v, status %q", src.acts, statusLine(m))
	}
}

func TestMessageAsksForTheServiceWhenThereAreSeveral(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, twoSvc)
	m = press(t, m, "m")
	if !strings.Contains(statusLine(m), "Which service? (api, web; enter = api)") {
		t.Fatalf("status = %q", statusLine(m))
	}
	m = typeText(t, m, "web")
	m = press(t, m, "enter")
	if !strings.Contains(statusLine(m), "Message to multi/web:") {
		t.Fatalf("status = %q", statusLine(m))
	}
	m = typeText(t, m, "héllo wörld")
	m = press(t, m, "backspace", "d", "enter")
	if strings.Join(src.acts, "|") != "message multi/web: héllo wörld" {
		t.Errorf("acts = %v", src.acts)
	}
	if !strings.Contains(statusLine(m), "sent to multi/web") {
		t.Errorf("status = %q", statusLine(m))
	}
}

func TestMessageToASingleServiceTicketSkipsTheServiceQuestion(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, oneSvc)
	m = press(t, m, "m")
	if !strings.Contains(statusLine(m), "Message to demo/api:") {
		t.Fatalf("status = %q", statusLine(m))
	}
	m = typeText(t, m, "hi")
	m = press(t, m, "enter")
	if strings.Join(src.acts, "|") != "message demo/api: hi" {
		t.Errorf("acts = %v", src.acts)
	}
}

func TestPromptsValidateAndCancel(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, twoSvc)

	// an unknown service names the choices and sends nothing
	m = press(t, m, "m")
	m = typeText(t, m, "db")
	m = press(t, m, "enter")
	if v := statusLine(m); !strings.Contains(v, `no service "db"`) || m.modal != nil {
		t.Errorf("status = %q", v)
	}
	// enter alone picks the first service
	m = press(t, m, "m", "enter")
	if !strings.Contains(statusLine(m), "Message to multi/api:") {
		t.Errorf("status = %q", statusLine(m))
	}
	// an empty message is refused
	m = press(t, m, "enter")
	if !strings.Contains(statusLine(m), "nothing to send") {
		t.Errorf("status = %q", statusLine(m))
	}
	// esc abandons a prompt
	m = press(t, m, "m", "esc")
	if m.modal != nil || len(src.acts) != 0 {
		t.Errorf("esc: modal %v, acts %v", m.modal != nil, src.acts)
	}
	// ctrl+c still quits from inside a prompt
	m = press(t, m, "m")
	if _, cmd := send(m, key("ctrl+c")); !isQuit(cmd) {
		t.Error("ctrl+c did not quit from a prompt")
	}
	// keys typed into a prompt are text, not commands
	m = press(t, m, "m", "esc", "m", "enter")
	m = typeText(t, m, "q?aj")
	if m.quitting || m.help || string(m.modal.input) != "q?aj" {
		t.Errorf("typing leaked into commands: input %q", string(m.modal.input))
	}
}

func TestContractAcceptAndRejectPickTheOpenProposal(t *testing.T) {
	open := api.ContractProposal{ID: "P-api-1", From: "api", Status: "open", What: "add a field\nmore"}
	done := api.ContractProposal{ID: "P-web-1", From: "web", Status: "accepted", What: "old"}

	// one open proposal: accept after a confirmation
	src := &fakeSource{proposals: []api.ContractProposal{done, open}}
	m := ready(t, src, twoSvc)
	m = press(t, m, "c")
	if v := statusLine(m); !strings.Contains(v, "Accept P-api-1 from api into the contract: add a field? [y/N]") {
		t.Fatalf("status = %q", v)
	}
	m = press(t, m, "n")
	if len(src.acts) != 0 {
		t.Fatalf("acted on a no: %v", src.acts)
	}
	m = press(t, m, "c", "y")
	if strings.Join(src.acts, "|") != `resolve multi P-api-1 accept=true reason=""` {
		t.Errorf("acts = %v", src.acts)
	}

	// reject needs a reason, and typing it is the confirmation
	src = &fakeSource{proposals: []api.ContractProposal{open}}
	m = ready(t, src, twoSvc)
	m = press(t, m, "C")
	if !strings.Contains(statusLine(m), "Reject P-api-1 from api — reason:") {
		t.Fatalf("status = %q", statusLine(m))
	}
	m = press(t, m, "enter") // no reason
	if len(src.acts) != 0 || !strings.Contains(statusLine(m), "needs a reason") {
		t.Errorf("an empty reason went through: %v, %q", src.acts, statusLine(m))
	}
	m = press(t, m, "C")
	m = typeText(t, m, "too big")
	m = press(t, m, "enter")
	if strings.Join(src.acts, "|") != `resolve multi P-api-1 accept=false reason="too big"` {
		t.Errorf("acts = %v", src.acts)
	}
}

func TestSeveralOpenProposalsAskWhichOne(t *testing.T) {
	src := &fakeSource{proposals: []api.ContractProposal{
		{ID: "P-api-1", From: "api", Status: "open", What: "one"},
		{ID: "P-web-1", From: "web", Status: "open", What: "two"},
	}}
	m := ready(t, src, twoSvc)
	m = press(t, m, "c")
	if !strings.Contains(statusLine(m), "Which proposal? (P-api-1, P-web-1; enter = P-api-1)") {
		t.Fatalf("status = %q", statusLine(m))
	}
	m = typeText(t, m, "P-nope-9")
	m = press(t, m, "enter")
	if len(src.acts) != 0 || !strings.Contains(statusLine(m), "not one of the open proposals") {
		t.Errorf("acts %v, status %q", src.acts, statusLine(m))
	}
	m = press(t, m, "c")
	m = typeText(t, m, "P-web-1")
	m = press(t, m, "enter", "y")
	if strings.Join(src.acts, "|") != `resolve multi P-web-1 accept=true reason=""` {
		t.Errorf("acts = %v", src.acts)
	}
}

func TestContractKeysExplainWhenThereIsNothingToDecide(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, twoSvc)
	m = press(t, m, "c")
	if !strings.Contains(statusLine(m), "has no open contract proposals") {
		t.Errorf("status = %q", statusLine(m))
	}
	src.actErr = map[string]error{"proposals": ErrMissing}
	m = press(t, m, "C")
	if !strings.Contains(statusLine(m), "has no contract") {
		t.Errorf("status = %q", statusLine(m))
	}
	src.actErr = map[string]error{"proposals": errors.New("daemon is sulking")}
	m = press(t, m, "c")
	if !strings.Contains(statusLine(m), "error: daemon is sulking") {
		t.Errorf("status = %q", statusLine(m))
	}
	if len(src.acts) != 0 {
		t.Errorf("acts = %v", src.acts)
	}
}

func TestAttachSuspendsTheTUIRunsTheCommandAndComesBack(t *testing.T) {
	src := &fakeSource{attachCmd: []string{"herdr", "attach", "pane-7"}}
	m := ready(t, src, oneSvc)
	var ran [][]string
	m.opts.Exec = func(argv []string, done func(error) tea.Msg) tea.Cmd {
		ran = append(ran, argv)
		return func() tea.Msg { return done(nil) }
	}
	src.tickets = []api.TicketSummary{oneSvc}
	m = press(t, m, "t")
	if len(ran) != 1 || strings.Join(ran[0], " ") != "herdr attach pane-7" {
		t.Fatalf("ran %v", ran)
	}
	if strings.Join(src.acts, "|") != "attach demo/api" {
		t.Errorf("acts = %v", src.acts)
	}
	if !strings.Contains(statusLine(m), "back from the agent") || !m.connected || len(m.tickets) != 1 {
		t.Errorf("not restored: status %q", statusLine(m))
	}
	if !strings.Contains(m.View(), "demo") {
		t.Errorf("the dashboard did not come back:\n%s", m.View())
	}

	// the attached program failing is reported, and the TUI still works
	m.opts.Exec = func(argv []string, done func(error) tea.Msg) tea.Cmd {
		return func() tea.Msg { return done(errors.New("exit status 1")) }
	}
	m = press(t, m, "t")
	if !strings.Contains(statusLine(m), "error: attach ended with an error: exit status 1") {
		t.Errorf("status = %q", statusLine(m))
	}
}

func TestAttachFailuresStayInTheTUI(t *testing.T) {
	src := &fakeSource{actErr: map[string]error{"attach": &api.Error{Code: api.CodeNotRunning, Message: "service api has no agent running"}}}
	m := ready(t, src, oneSvc)
	called := false
	m.opts.Exec = func([]string, func(error) tea.Msg) tea.Cmd { called = true; return nil }
	m = press(t, m, "t")
	if called || !strings.Contains(statusLine(m), "error: service api has no agent running") {
		t.Errorf("called %v, status %q", called, statusLine(m))
	}
	src.actErr, src.attachCmd = nil, nil
	m = press(t, m, "t")
	if called || !strings.Contains(statusLine(m), "no attach command") {
		t.Errorf("an empty command: called %v, status %q", called, statusLine(m))
	}
}

func TestPushReportsPartialFailures(t *testing.T) {
	src := &fakeSource{pushRes: api.PushRunResult{Services: []api.PushServiceResult{
		{Service: "api", Pushed: true}, {Service: "web", Error: "remote rejected"}, {Service: "db", Skipped: true},
	}}}
	m := ready(t, src, oneSvc)
	m = press(t, m, "p", "y")
	if v := statusLine(m); !strings.Contains(v, "2 of 3 services pushed") || !strings.Contains(v, "web: remote rejected") {
		t.Errorf("status = %q", v)
	}
}

func TestActionKeysDoNothingWhileHelpIsOpenOrTheDaemonIsDown(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, oneSvc)
	m = press(t, m, "?", "a", "p", "x")
	if m.modal != nil || len(src.acts) != 0 {
		t.Error("an action started under the help overlay")
	}
	down := New(Options{Source: &fakeSource{err: errors.New("down")}})
	down, _ = load(t, down)
	down = press(t, down, "a", "x")
	if down.modal != nil {
		t.Error("an action started with no daemon")
	}
}

func TestActionKeysWorkOnAnEmptyFleetWithoutPanicking(t *testing.T) {
	m := ready(t, &fakeSource{})
	m = press(t, m, "a", "v", "p", "x", "t", "m", "c", "C")
	if m.modal != nil || !strings.Contains(statusLine(m), "no ticket selected") {
		t.Errorf("status = %q", statusLine(m))
	}
}

func TestASuccessfulActionRefreshesTheFleetAndTheOpenTab(t *testing.T) {
	src := &fakeSource{files: map[string]string{"plan.md": "before\n"}}
	m := ready(t, src, oneSvc)
	m = press(t, m, "enter")
	if !strings.Contains(m.View(), "before") {
		t.Fatalf("view:\n%s", m.View())
	}
	src.files["plan.md"] = "after\n"
	src.tickets = []api.TicketSummary{{ID: "demo", Title: "Demo", Phase: domain.PhaseImplement, Services: oneSvc.Services}}
	m = press(t, m, "a") // the open ticket's row is oneSvc, with a gate pending
	m = press(t, m, "y")
	v := m.View()
	if !strings.Contains(v, "after") || strings.Contains(v, "before") {
		t.Errorf("the open tab was not refreshed:\n%s", v)
	}
	if m.tickets[0].Phase != domain.PhaseImplement {
		t.Errorf("the fleet was not refreshed: %+v", m.tickets)
	}
}
