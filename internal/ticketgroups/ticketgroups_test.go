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
	if got := ticketgroups.Header(groups[1], false); got != "▾ implement (2) · $4.12 · agents 1h20m" {
		t.Errorf("header = %q", got)
	}
	if got := ticketgroups.Header(groups[2], true); got != "▸ done (1) · $1.50 · agents 10m" {
		t.Errorf("folded header = %q", got)
	}
	if got := ticketgroups.Header(groups[0], false); got != "▾ plan (1) · agents 0s" {
		t.Errorf("a group where no cost is reported shows none: %q", got)
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
	if f := ticketgroups.Footer(got); f != "2 tickets · $3.00 · agents 3h00m · span 2h00m" {
		t.Errorf("footer = %q", f)
	}
	done := []api.TicketSummary{tk("a", domain.PhaseDone, 1, 60, 0, time.Hour), tk("b", domain.PhaseDone, 1, 60, 10*time.Minute, 90*time.Minute)}
	if got := ticketgroups.TotalsOf(done, now); got.ElapsedSeconds != 5400 {
		t.Errorf("all done: the span ends at the last done, got %ds", got.ElapsedSeconds)
	}
}
