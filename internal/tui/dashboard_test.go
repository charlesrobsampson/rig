package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func svc(name string, state domain.AgentState, port int) api.ServiceSummary {
	return api.ServiceSummary{Name: name, AgentState: state, HostPort: port, Status: domain.ServiceRunning}
}

func ids(d *dashboard) string {
	var out []string
	for _, r := range d.rows {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

func TestRowsSortByAttentionThenByID(t *testing.T) {
	d := &dashboard{}
	d.set([]api.TicketSummary{
		{ID: "a-quiet", Phase: domain.PhaseImplement, Services: []api.ServiceSummary{svc("api", domain.AgentWorking, 0)}},
		{ID: "b-gate", Phase: domain.PhasePlan, PendingGate: domain.GatePlanApproval},
		{ID: "c-error", Error: "ticket.yaml not found"},
		{ID: "d-contract", ContractPending: true},
		{ID: "e-blocked", Services: []api.ServiceSummary{svc("api", domain.AgentBlocked, 0)}},
		{ID: "f-violation", ContractViolation: true},
		{ID: "g-quiet"},
		{ID: "h-blocked", Services: []api.ServiceSummary{svc("api", domain.AgentBlocked, 0)}},
	})
	if got, want := ids(d), "e-blocked,h-blocked,f-violation,d-contract,c-error,b-gate,a-quiet,g-quiet"; got != want {
		t.Errorf("order = %s\nwant    %s", got, want)
	}
}

func TestEveryNeedRendersItsMarker(t *testing.T) {
	tests := []struct {
		name string
		t    api.TicketSummary
		want []string
	}{
		{"blocked agent", api.TicketSummary{ID: "t", Services: []api.ServiceSummary{svc("api", domain.AgentBlocked, 0)}}, []string{"BLOCKED", "! api"}},
		{"gate", api.TicketSummary{ID: "t", PendingGate: domain.GateReviewApproval}, []string{"APPROVE review_approval"}},
		{"contract change", api.TicketSummary{ID: "t", ContractPending: true}, []string{"CONTRACT CHANGE"}},
		{"contract edited", api.TicketSummary{ID: "t", ContractViolation: true}, []string{"CONTRACT EDITED"}},
		{"ticket error", api.TicketSummary{ID: "t", Title: "T", Error: "state.json not found"}, []string{"ERROR", "T  ! state.json not found"}},
		{"service error", api.TicketSummary{ID: "t", Services: []api.ServiceSummary{{Name: "api", Status: domain.ServiceError}}}, []string{"ERROR"}},
		{"several at once", api.TicketSummary{ID: "t", ContractPending: true, PendingGate: domain.GatePlanApproval}, []string{"CONTRACT CHANGE, APPROVE plan_approval"}},
		{"nothing needed", api.TicketSummary{ID: "t", Services: []api.ServiceSummary{svc("api", domain.AgentWorking, 4000)}}, []string{"● api", "4000"}},
	}
	for _, tt := range tests {
		d := &dashboard{}
		d.set([]api.TicketSummary{tt.t})
		v := d.view(120, 10)
		for _, want := range tt.want {
			if !strings.Contains(v, want) {
				t.Errorf("%s: view lacks %q:\n%s", tt.name, want, v)
			}
		}
	}
	d := &dashboard{}
	d.set([]api.TicketSummary{{ID: "t", Services: []api.ServiceSummary{svc("api", domain.AgentWorking, 4000)}}})
	if strings.Contains(d.view(120, 10), "BLOCKED") || strings.Contains(d.view(120, 10), "APPROVE") {
		t.Error("a ticket that needs nothing shows a marker")
	}
}

func TestTheLightsShowEachAgentState(t *testing.T) {
	want := map[domain.AgentState]string{
		domain.AgentWorking: "●", domain.AgentIdle: "○", domain.AgentBlocked: "!", domain.AgentExited: "✕", domain.AgentUnknown: "·", "": "·",
	}
	for s, glyph := range want {
		if Light(s) != glyph {
			t.Errorf("Light(%q) = %q, want %q", s, Light(s), glyph)
		}
	}
	d := &dashboard{}
	d.set([]api.TicketSummary{{ID: "t", Services: []api.ServiceSummary{svc("api", domain.AgentWorking, 0), svc("web", domain.AgentIdle, 0)}}})
	if v := d.view(100, 5); !strings.Contains(v, "● api  ○ web") {
		t.Errorf("view:\n%s", v)
	}
}

func TestTheSelectionStaysOnTheSameTicketWhenRowsReorder(t *testing.T) {
	d := &dashboard{}
	quiet := api.TicketSummary{ID: "b-quiet"}
	other := api.TicketSummary{ID: "a-other"}
	d.set([]api.TicketSummary{other, quiet})
	d.move(1)
	if d.selected != "b-quiet" {
		t.Fatalf("selected = %s", d.selected)
	}
	// b-quiet now needs attention and jumps above a-other; the selection must follow it
	quiet.ContractPending = true
	d.set([]api.TicketSummary{other, quiet})
	if ids(d) != "b-quiet,a-other" || d.selected != "b-quiet" {
		t.Errorf("order %s, selected %s", ids(d), d.selected)
	}
}

func TestTheSelectionFallsToTheNeighbourWhenItsTicketDisappears(t *testing.T) {
	d := &dashboard{}
	d.set([]api.TicketSummary{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	d.move(1)
	d.set([]api.TicketSummary{{ID: "a"}, {ID: "c"}})
	if d.selected != "c" {
		t.Errorf("selected = %s, want the row that took b's place", d.selected)
	}
	d.last()
	d.set([]api.TicketSummary{{ID: "a"}})
	if d.selected != "a" {
		t.Errorf("selected = %s, want the last row", d.selected)
	}
	d.set(nil)
	if d.selected != "" {
		t.Errorf("selected = %q with an empty fleet", d.selected)
	}
	d.set([]api.TicketSummary{{ID: "z"}})
	if d.selected != "z" {
		t.Errorf("selected = %q, want the first ticket to appear", d.selected)
	}
}

func TestNavigationStopsAtTheEnds(t *testing.T) {
	m, _ := load(t, New(Options{Source: &fakeSource{tickets: []api.TicketSummary{{ID: "a"}, {ID: "b"}, {ID: "c"}}}}))
	selected := func() string { return m.dash.selected }
	if selected() != "a" {
		t.Fatalf("starts on %s", selected())
	}
	for _, step := range []struct {
		key  string
		want string
	}{{"k", "a"}, {"up", "a"}, {"j", "b"}, {"down", "c"}, {"j", "c"}, {"j", "c"}, {"g", "a"}, {"G", "c"}, {"home", "a"}, {"end", "c"}} {
		switch step.key {
		case "up":
			m, _ = send(m, tea.KeyMsg{Type: tea.KeyUp})
		case "down":
			m, _ = send(m, tea.KeyMsg{Type: tea.KeyDown})
		case "home":
			m, _ = send(m, tea.KeyMsg{Type: tea.KeyHome})
		case "end":
			m, _ = send(m, tea.KeyMsg{Type: tea.KeyEnd})
		default:
			m, _ = send(m, key(step.key))
		}
		if selected() != step.want {
			t.Fatalf("after %s: on %s, want %s", step.key, selected(), step.want)
		}
	}
}

func TestNavigationOnAnEmptyFleetDoesNothing(t *testing.T) {
	m, _ := load(t, New(Options{Source: &fakeSource{}}))
	for _, k := range []string{"j", "k", "g", "G"} {
		m, _ = send(m, key(k))
	}
	if !strings.Contains(m.View(), "no tickets yet") {
		t.Errorf("view:\n%s", m.View())
	}
}

func TestLongFleetsScrollAndKeepTheSelectionVisible(t *testing.T) {
	var fleet []api.TicketSummary
	for i := 0; i < 30; i++ {
		fleet = append(fleet, api.TicketSummary{ID: "t" + string(rune('a'+i/10)) + string(rune('0'+i%10))})
	}
	m, _ := load(t, New(Options{Source: &fakeSource{tickets: fleet}}))
	m, _ = send(m, tea.WindowSizeMsg{Width: 100, Height: 10}) // header + 6 rows visible + column line
	for range 20 {
		m, _ = send(m, key("j"))
	}
	v := m.View()
	if !strings.Contains(v, "> tc0") && !strings.Contains(v, "> "+m.dash.selected) {
		t.Errorf("the selected row is not on screen:\n%s", v)
	}
	if strings.Contains(v, "ta0") {
		t.Errorf("the first row should have scrolled off:\n%s", v)
	}
	if n := len(strings.Split(v, "\n")); n > 10 {
		t.Errorf("%d lines on a 10-line screen", n)
	}
	m, _ = send(m, key("g"))
	if v := m.View(); !strings.Contains(v, "> ta0") {
		t.Errorf("after g the first row is not selected on screen:\n%s", v)
	}
}

func TestTheSelectedRowIsMarked(t *testing.T) {
	d := &dashboard{}
	d.set([]api.TicketSummary{{ID: "a"}, {ID: "b"}})
	d.move(1)
	v := d.view(80, 5)
	if !strings.Contains(v, "> b") || strings.Contains(v, "> a") {
		t.Errorf("view:\n%s", v)
	}
}
