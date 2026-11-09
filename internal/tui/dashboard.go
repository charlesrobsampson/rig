package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// Need is why a ticket wants the human's attention, most urgent first. A ticket's rank
// is its most urgent need; tickets that need nothing sort below all of those.
type Need int

const (
	NeedBlocked           Need = iota + 1 // an agent waits on a permission prompt
	NeedContractViolation                 // the agreed contract was edited outside rig
	NeedContractChange                    // a proposed contract change awaits a decision
	NeedError                             // the ticket's files or a service are broken
	NeedDrift                             // what exists no longer matches state.json
	NeedGate                              // an approval stands between the ticket and its next phase
)

// Marker is the short label a need shows in the table.
func (n Need) Marker() string {
	switch n {
	case NeedBlocked:
		return "BLOCKED"
	case NeedContractViolation:
		return "CONTRACT EDITED"
	case NeedContractChange:
		return "CONTRACT CHANGE"
	case NeedError:
		return "ERROR"
	case NeedDrift:
		return "DRIFT"
	case NeedGate:
		return "APPROVE"
	}
	return ""
}

// Needs lists what a ticket needs from the human, most urgent first.
func Needs(t api.TicketSummary) []Need {
	var out []Need
	for _, s := range t.Services {
		if s.AgentState == domain.AgentBlocked {
			out = append(out, NeedBlocked)
			break
		}
	}
	if t.ContractViolation {
		out = append(out, NeedContractViolation)
	}
	if t.ContractPending {
		out = append(out, NeedContractChange)
	}
	failed := t.Error != ""
	for _, s := range t.Services {
		failed = failed || s.Status == domain.ServiceError
	}
	if failed {
		out = append(out, NeedError)
	}
	if len(t.Drift) > 0 {
		out = append(out, NeedDrift)
	}
	if t.PendingGate != "" {
		out = append(out, NeedGate)
	}
	return out
}

// Light is the glyph for an agent's state.
func Light(s domain.AgentState) string {
	switch s {
	case domain.AgentWorking:
		return "●"
	case domain.AgentIdle:
		return "○"
	case domain.AgentBlocked:
		return "!"
	case domain.AgentExited:
		return "✕"
	}
	return "·"
}

// dashboard is the fleet table: tickets that need the human first, then the rest by
// id, with one row selected. Selection follows the ticket, not the row, so a refresh
// that reorders rows does not move it onto another ticket.
type dashboard struct {
	rows     []api.TicketSummary // sorted
	selected string              // ticket id
	top      int                 // first visible row
}

// rank is the sort key: the most urgent need, or beyond every need when there is none.
func rank(t api.TicketSummary) Need {
	if n := Needs(t); len(n) > 0 {
		return n[0]
	}
	return NeedGate + 1
}

// set replaces the fleet, keeping the selection on the same ticket. If that ticket is
// gone, the selection falls to the row now at its old position (or the last row).
func (d *dashboard) set(tickets []api.TicketSummary) {
	oldIndex := d.index()
	rows := append([]api.TicketSummary(nil), tickets...)
	sort.SliceStable(rows, func(i, j int) bool {
		if ri, rj := rank(rows[i]), rank(rows[j]); ri != rj {
			return ri < rj
		}
		return rows[i].ID < rows[j].ID
	})
	d.rows = rows
	if len(rows) == 0 {
		d.selected, d.top = "", 0
		return
	}
	if d.index() >= 0 {
		return
	}
	if oldIndex < 0 || oldIndex >= len(rows) {
		oldIndex = min(max(oldIndex, 0), len(rows)-1)
	}
	d.selected = rows[oldIndex].ID
}

// index is the selected row, or -1.
func (d *dashboard) index() int {
	for i, r := range d.rows {
		if r.ID == d.selected {
			return i
		}
	}
	return -1
}

// move changes the selection by delta rows, stopping at the first and last.
func (d *dashboard) move(delta int) {
	if len(d.rows) == 0 {
		return
	}
	i := d.index()
	if i < 0 {
		i = 0
	}
	d.selected = d.rows[min(max(i+delta, 0), len(d.rows)-1)].ID
}

func (d *dashboard) first() {
	if len(d.rows) > 0 {
		d.selected = d.rows[0].ID
	}
}

func (d *dashboard) last() {
	if len(d.rows) > 0 {
		d.selected = d.rows[len(d.rows)-1].ID
	}
}

// current is the selected ticket.
func (d *dashboard) current() (api.TicketSummary, bool) {
	if i := d.index(); i >= 0 {
		return d.rows[i], true
	}
	return api.TicketSummary{}, false
}

// view draws the table in a w x h box: a header line, then as many rows as fit with the
// selected one always visible.
func (d *dashboard) view(w, h int) string {
	if len(d.rows) == 0 {
		return "no tickets yet — create one with: rig new"
	}
	cells := make([][6]string, len(d.rows))
	widths := [6]int{len("TICKET"), len("PHASE"), len("AGENTS"), len("PORTS"), len("COST"), len("NEEDS")}
	for i, t := range d.rows {
		cells[i] = [6]string{t.ID, phaseCell(t), agentsCell(t), portsCell(t), costCell(t), needsCell(t)}
		for c, s := range cells[i] {
			widths[c] = max(widths[c], len([]rune(s)))
		}
	}
	line := func(sel bool, c [6]string, title string) string {
		prefix := "  "
		if sel {
			prefix = "> "
		}
		var b strings.Builder
		b.WriteString(prefix)
		for i, s := range c {
			b.WriteString(pad(s, widths[i]) + "  ")
		}
		b.WriteString(title)
		return strings.TrimRight(b.String(), " ")
	}
	out := []string{line(false, [6]string{"TICKET", "PHASE", "AGENTS", "PORTS", "COST", "NEEDS"}, "TITLE")}

	pane := d.summaryPane(w, h)
	visible := h - 1 - len(pane)
	if visible < 1 {
		visible = 1
	}
	i := d.index()
	if i < d.top {
		d.top = i
	}
	if i >= d.top+visible {
		d.top = i - visible + 1
	}
	d.top = min(max(d.top, 0), max(len(d.rows)-visible, 0))
	for r := d.top; r < len(d.rows) && r < d.top+visible; r++ {
		t := d.rows[r]
		title := t.Title
		if t.Error != "" {
			title = strings.TrimSpace(title + "  ! " + t.Error)
		}
		out = append(out, line(r == i, cells[r], title))
	}
	return strings.Join(append(out, pane...), "\n")
}

// maxSummaryLines bounds the summary pane's text.
const maxSummaryLines = 5

// summaryPane is the language-model summary of the selected ticket, drawn under the
// table: labelled advisory, with the real diff stats it was made from beside it, and
// marked REMOTE when the ticket's content was sent off this machine. It is left out when
// the screen is too short to keep a useful table above it.
func (d *dashboard) summaryPane(w, h int) []string {
	t, ok := d.current()
	if !ok || t.AISummary == nil || h < 14 {
		return nil
	}
	s := t.AISummary
	backend := s.Backend
	if s.Model != "" {
		backend += " " + s.Model
	}
	if s.Remote {
		backend += " · REMOTE"
	}
	head := fmt.Sprintf("── summary (advisory) · %s · %d files +%d -%d ──", backend, s.Files, s.Added, s.Deleted)
	lines := []string{ansi.Truncate(head, w, "…")}
	var body []string
	for _, l := range wrapLines(strings.TrimSpace(s.Text), w) {
		if strings.TrimSpace(l) != "" {
			body = append(body, l)
		}
	}
	if len(body) > maxSummaryLines {
		body = append(body[:maxSummaryLines-1], "…")
	}
	lines = append(lines, body...)
	if s.Error != "" {
		note := "summary refresh failed: "
		if s.Text != "" {
			note = "out of date, refresh failed: "
		}
		lines = append(lines, ansi.Truncate(note+s.Error, w, "…"))
	}
	return lines
}

// remoteSummaries names the backend when any ticket's summary was made by a model off
// this machine, so the header can say so.
func remoteSummaries(tickets []api.TicketSummary) (backend string, ok bool) {
	for _, t := range tickets {
		if t.AISummary != nil && t.AISummary.Remote {
			return t.AISummary.Backend, true
		}
	}
	return "", false
}

func pad(s string, w int) string {
	if n := len([]rune(s)); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func phaseCell(t api.TicketSummary) string {
	if t.Phase == "" {
		return "-"
	}
	return string(t.Phase)
}

// agentsCell is one light per service: "● api  ○ web".
func agentsCell(t api.TicketSummary) string {
	if len(t.Services) == 0 {
		return "-"
	}
	parts := make([]string, len(t.Services))
	for i, s := range t.Services {
		parts[i] = Light(s.AgentState) + " " + s.Name
	}
	return strings.Join(parts, "  ")
}

func portsCell(t api.TicketSummary) string {
	var ports []string
	for _, s := range t.Services {
		if s.HostPort != 0 {
			ports = append(ports, strconv.Itoa(s.HostPort))
		}
	}
	if len(ports) == 0 {
		return "-"
	}
	return strings.Join(ports, ",")
}

// costCell is what the ticket's agents have spent, summed, or "-" when no harness has
// reported a cost (an unreported cost is not shown as $0.00).
func costCell(t api.TicketSummary) string {
	total, any := 0.0, false
	for _, s := range t.Services {
		if s.CostUSD != nil {
			total, any = total+*s.CostUSD, true
		}
	}
	if !any {
		return "-"
	}
	return fmt.Sprintf("$%.2f", total)
}

// needsCell lists the markers; a pending gate names the gate.
func needsCell(t api.TicketSummary) string {
	var parts []string
	for _, n := range Needs(t) {
		m := n.Marker()
		if n == NeedGate {
			m = fmt.Sprintf("%s %s", m, t.PendingGate)
		}
		parts = append(parts, m)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}
