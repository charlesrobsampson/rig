// Package ticketgroups groups a vessel's tickets by phase and totals them, for the TUI
// dashboard and rig list --group, so the two cannot disagree.
package ticketgroups

import (
	"fmt"
	"strconv"
	"strings"
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

// Row is the cells a group header or the footer puts under the table's columns, as the
// text the ticket rows use. A cell that cannot be totalled and is not the same for every
// ticket is blank, so it is blank here and nothing says "-" for it.
type Row struct {
	Label    string // "▸ done (34)" or "all (19)": the first column
	Services string // the services' status when every ticket's is the same
	Cost     string // the sum, blank when no ticket reports one
	Time     string // the span from the first created to the last done, or to now
	Agent    string // the agent time, summed
	Ports    string // when every ticket's are the same
	Title    string // when every ticket's is the same
}

// GroupRow is the header row of a group; folded shows ▸, else ▾.
func GroupRow(g Group, folded bool, now time.Time) Row {
	mark := "▾"
	if folded {
		mark = "▸"
	}
	return rowOf(fmt.Sprintf("%s %s (%d)", mark, g.Phase, len(g.Tickets)), g.Tickets, now)
}

// TotalRow is the footer row for the whole vessel: "all (19)".
func TotalRow(tickets []api.TicketSummary, now time.Time) Row {
	return rowOf(fmt.Sprintf("all (%d)", len(tickets)), tickets, now)
}

func rowOf(label string, tickets []api.TicketSummary, now time.Time) Row {
	t := TotalsOf(tickets, now)
	r := Row{
		Label:    label,
		Services: Common(tickets, func(k api.TicketSummary) string { return ServicesText(k.Services) }),
		Agent:    Duration(t.AgentSeconds),
		Ports:    Common(tickets, func(k api.TicketSummary) string { return PortsText(k.Services) }),
		Title:    Common(tickets, TitleText),
	}
	if t.Costed {
		r.Cost = fmt.Sprintf("$%.2f", t.CostUSD)
	}
	if t.Dated {
		r.Time = Duration(t.ElapsedSeconds)
	}
	return r
}

// Common is the value f gives every ticket when it is the same for all of them, else "".
// "-" (nothing to show) counts as no value.
func Common(tickets []api.TicketSummary, f func(api.TicketSummary) string) string {
	if len(tickets) == 0 {
		return ""
	}
	v := f(tickets[0])
	for _, t := range tickets[1:] {
		if f(t) != v {
			return ""
		}
	}
	if v == "-" {
		return ""
	}
	return v
}

// ServicesText is a ticket's services as "api:running/idle, web:stopped", "-" for none.
func ServicesText(services []api.ServiceSummary) string {
	if len(services) == 0 {
		return "-"
	}
	parts := make([]string, len(services))
	for i, s := range services {
		status := string(s.Status)
		if status == "" {
			status = "-"
		}
		if s.AgentState != "" && s.AgentState != domain.AgentUnknown {
			status += "/" + string(s.AgentState)
		}
		parts[i] = s.Name + ":" + status
	}
	return strings.Join(parts, ", ")
}

// PortsText is the services' host ports, comma-joined, "-" for none.
func PortsText(services []api.ServiceSummary) string {
	var ports []string
	for _, s := range services {
		if s.HostPort != 0 {
			ports = append(ports, strconv.Itoa(s.HostPort))
		}
	}
	if len(ports) == 0 {
		return "-"
	}
	return strings.Join(ports, ",")
}

// TitleText is the ticket's title with its error after it.
func TitleText(t api.TicketSummary) string {
	switch {
	case t.Error == "":
		return t.Title
	case t.Title == "":
		return "! " + t.Error
	}
	return t.Title + "  ! " + t.Error
}

// CostText is a ticket's cost, "-" when no harness reported one.
func CostText(usd *float64) string {
	if usd == nil {
		return "-"
	}
	return fmt.Sprintf("$%.2f", *usd)
}

// ElapsedText is the ticket's age: from creation to done, or to now while it is open.
func ElapsedText(t api.TicketSummary, now time.Time) string {
	if t.CreatedAt == nil {
		return "-"
	}
	end := now
	if t.DoneAt != nil {
		end = *t.DoneAt
	}
	return Duration(int64(end.Sub(*t.CreatedAt).Seconds()))
}

// AgentText is a ticket's agent time, "-" for none.
func AgentText(seconds int64) string {
	if seconds == 0 {
		return "-"
	}
	return Duration(seconds)
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
	Dated          bool // some ticket has a creation time, so the span means something
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
	t.Dated = !first.IsZero()
	if !first.IsZero() && end.After(first) {
		t.ElapsedSeconds = int64(end.Sub(first).Seconds())
	}
	return t
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
