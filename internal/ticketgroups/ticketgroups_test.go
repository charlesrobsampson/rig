package ticketgroups_test

import (
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/ticketgroups"
)

var t0 = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

func tk(id string, phase domain.Phase, cost float64, agent int64, created, done time.Duration) api.TicketSummary {
	s := api.TicketSummary{ID: id, Phase: phase, AgentSeconds: agent}
	if cost >= 0 {
		s.CostUSD = &cost
	}
	c := t0.Add(created)
	s.CreatedAt = &c
	if done > 0 {
		d := t0.Add(done)
		s.DoneAt = &d
	}
	return s
}

func TestGroupsFollowTheLifecycleAndSumTheirTickets(t *testing.T) {
	groups := ticketgroups.Of([]api.TicketSummary{
		tk("d1", domain.PhaseDone, 1.5, 600, 0, time.Hour),
		tk("i1", domain.PhaseImplement, 2, 3000, 0, 0),
		{ID: "broken"},
		tk("i2", domain.PhaseImplement, 2.12, 1800, 0, 0),
		tk("p1", domain.PhasePlan, -1, 0, 0, 0),
	})
	var order []domain.Phase
	for _, g := range groups {
		order = append(order, g.Phase)
	}
	want := []domain.Phase{domain.PhasePlan, domain.PhaseImplement, domain.PhaseDone, ticketgroups.Unknown}
	if len(order) != len(want) {
		t.Fatalf("groups = %v, want %v (empty groups are left out)", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("groups = %v, want %v", order, want)
		}
	}
	now := t0.Add(2 * time.Hour)
	impl := ticketgroups.GroupRow(groups[1], false, now)
	if impl.Label != "▾ implement (2)" || impl.Cost != "$4.12" || impl.Agent != "1h20m" || impl.Time != "2h00m" {
		t.Errorf("implement row = %+v", impl)
	}
	if done := ticketgroups.GroupRow(groups[2], true, now); done.Label != "▸ done (1)" || done.Cost != "$1.50" || done.Agent != "10m" || done.Time != "1h00m" {
		t.Errorf("a done group spans to its last done: %+v", done)
	}
	if plan := ticketgroups.GroupRow(groups[0], false, now); plan.Cost != "" {
		t.Errorf("a group where no cost is reported shows none: %+v", plan)
	}
}

func svc(name string, st domain.ServiceStatus, a domain.AgentState, port int) api.ServiceSummary {
	return api.ServiceSummary{Name: name, Status: st, AgentState: a, HostPort: port}
}

func TestRowShowsWhatIsTheSameForEveryTicketAndBlanksTheRest(t *testing.T) {
	now := t0.Add(time.Hour)
	a := tk("a", domain.PhaseDone, 1, 60, 0, time.Hour)
	b := tk("b", domain.PhaseDone, 2, 60, 0, time.Hour)
	for _, k := range []*api.TicketSummary{&a, &b} {
		k.Title = "same"
		k.Services = []api.ServiceSummary{svc("rig", "stopped", "exited", 0)}
	}
	uniform := ticketgroups.GroupRow(ticketgroups.Group{Phase: domain.PhaseDone, Tickets: []api.TicketSummary{a, b}}, true, now)
	if uniform.Services != "rig:stopped/exited" || uniform.Title != "same" || uniform.Ports != "" {
		t.Errorf("uniform = %+v: common services and title show, no ports stays blank", uniform)
	}
	b.Title = "other"
	b.Services = []api.ServiceSummary{svc("rig", "running", "idle", 8080)}
	a.Services[0].HostPort = 8080
	mixed := ticketgroups.GroupRow(ticketgroups.Group{Phase: domain.PhaseDone, Tickets: []api.TicketSummary{a, b}}, false, now)
	if mixed.Services != "" || mixed.Title != "" || mixed.Ports != "8080" {
		t.Errorf("mixed = %+v: services and title differ so blank, the ports are the same so shown", mixed)
	}
}

func TestFooterRowIsLabelledAllAndSpansTheVessel(t *testing.T) {
	now := t0.Add(2 * time.Hour)
	r := ticketgroups.TotalRow([]api.TicketSummary{tk("a", domain.PhaseDone, 1, 60, 0, time.Hour), tk("b", domain.PhaseImplement, 2, 120, 30*time.Minute, 0)}, now)
	if r.Label != "all (2)" || r.Cost != "$3.00" || r.Agent != "3m" || r.Time != "2h00m" {
		t.Errorf("footer = %+v: one ticket is open, so the span runs to now", r)
	}
}

func TestTotalsSumAgentTimeButSpanTheElapsedTime(t *testing.T) {
	now := t0.Add(2 * time.Hour)
	open := []api.TicketSummary{
		tk("a", domain.PhaseImplement, 1, 5400, 0, 0),
		tk("b", domain.PhaseImplement, 2, 5400, 30*time.Minute, 0), // ran in parallel with a
	}
	got := ticketgroups.TotalsOf(open, now)
	if got.AgentSeconds != 10800 || got.ElapsedSeconds != 7200 || got.CostUSD != 3 || got.Tickets != 2 {
		t.Errorf("totals = %+v: agent time is the sum (3h), the span is 2h", got)
	}
	done := []api.TicketSummary{tk("a", domain.PhaseDone, 1, 60, 0, time.Hour), tk("b", domain.PhaseDone, 1, 60, 10*time.Minute, 90*time.Minute)}
	if got := ticketgroups.TotalsOf(done, now); got.ElapsedSeconds != 5400 {
		t.Errorf("all done: the span ends at the last done, got %ds", got.ElapsedSeconds)
	}
}
