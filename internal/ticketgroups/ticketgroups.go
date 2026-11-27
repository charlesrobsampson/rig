// Package ticketgroups groups a vessel's tickets by phase and totals them, for the TUI
// dashboard and rig list --group, so the two cannot disagree.
package ticketgroups

import (
	"fmt"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// Unknown is the group of tickets with no readable phase.
const Unknown domain.Phase = "unknown"

// Order is the lifecycle order groups are listed in; Unknown comes last.
var Order = []domain.Phase{domain.PhasePlan, domain.PhaseImplement, domain.PhaseTest, domain.PhaseReview, domain.PhasePush, domain.PhaseDone}

// Group is the tickets in one phase, in the order given, with their totals.
type Group struct {
	Phase        domain.Phase
	Tickets      []api.TicketSummary
	CostUSD      float64
	Costed       bool // some ticket reported a cost
	AgentSeconds int64
}

// Of returns the non-empty groups in lifecycle order.
func Of(tickets []api.TicketSummary) []Group {
	by := map[domain.Phase]*Group{}
	for _, t := range tickets {
		p := PhaseOf(t)
		g := by[p]
		if g == nil {
			g = &Group{Phase: p}
			by[p] = g
		}
		g.Tickets = append(g.Tickets, t)
		if t.CostUSD != nil {
			g.CostUSD, g.Costed = g.CostUSD+*t.CostUSD, true
		}
		g.AgentSeconds += t.AgentSeconds
	}
	var out []Group
	for _, p := range append(append([]domain.Phase(nil), Order...), Unknown) {
		if g := by[p]; g != nil {
			out = append(out, *g)
		}
	}
	return out
}

// PhaseOf is the group a ticket belongs to.
func PhaseOf(t api.TicketSummary) domain.Phase {
	for _, p := range Order {
		if t.Phase == p {
			return p
		}
	}
	return Unknown
}

// Header is the group's one line: "▾ implement (3) · $4.12 · agents 1h20m", with ▸ when folded.
func Header(g Group, folded bool) string {
	mark := "▾"
	if folded {
		mark = "▸"
	}
	line := fmt.Sprintf("%s %s (%d)", mark, g.Phase, len(g.Tickets))
	if g.Costed {
		line += fmt.Sprintf(" · $%.2f", g.CostUSD)
	}
	return line + " · agents " + Duration(g.AgentSeconds)
}

// Totals is a vessel's tickets, what they cost, the time its agents spent working
// (added up, so parallel tickets count each) and the span from the first ticket's
// creation to now, or to the last done when every ticket is done.
type Totals struct {
	Tickets        int
	CostUSD        float64
	Costed         bool
	AgentSeconds   int64
	ElapsedSeconds int64
}

func TotalsOf(tickets []api.TicketSummary, now time.Time) Totals {
	t := Totals{Tickets: len(tickets)}
	var first, lastDone time.Time
	allDone := len(tickets) > 0
	for _, k := range tickets {
		if k.CostUSD != nil {
			t.CostUSD, t.Costed = t.CostUSD+*k.CostUSD, true
		}
		t.AgentSeconds += k.AgentSeconds
		if k.CreatedAt != nil && (first.IsZero() || k.CreatedAt.Before(first)) {
			first = *k.CreatedAt
		}
		if k.DoneAt == nil {
			allDone = false
		} else if k.DoneAt.After(lastDone) {
			lastDone = *k.DoneAt
		}
	}
	end := now
	if allDone {
		end = lastDone
	}
	if !first.IsZero() && end.After(first) {
		t.ElapsedSeconds = int64(end.Sub(first).Seconds())
	}
	return t
}

// Footer is the vessel's totals on one line.
func Footer(t Totals) string {
	noun := "tickets"
	if t.Tickets == 1 {
		noun = "ticket"
	}
	line := fmt.Sprintf("%d %s", t.Tickets, noun)
	if t.Costed {
		line += fmt.Sprintf(" · $%.2f", t.CostUSD)
	}
	return line + fmt.Sprintf(" · agents %s · span %s", Duration(t.AgentSeconds), Duration(t.ElapsedSeconds))
}

// Duration is a short human form: 45s, 12m, 1h05m, 3d04h.
func Duration(seconds int64) string {
	d := time.Duration(max(seconds, 0)) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}
