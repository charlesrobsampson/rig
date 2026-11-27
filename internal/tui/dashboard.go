package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/ticketgroups"
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
	NeedRequest                           // an agent asked to move the ticket to another phase
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
	case NeedRequest:
		return "REQUEST"
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
	if t.PhaseRequest != nil {
		out = append(out, NeedRequest)
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

// Folds is which phase groups the person has folded (true) or opened (false); a group
// not in it is open, except done, which starts folded.
type Folds map[domain.Phase]bool

func (f Folds) folded(p domain.Phase) bool {
	if v, ok := f[p]; ok {
		return v
	}
	return p == domain.PhaseDone
}

// dashboard is the fleet table: tickets grouped by phase in lifecycle order, each group
// under a header line that folds, and inside a group the tickets that need the human
// first, then by id. The cursor is on a ticket (selected) or a group header (header).
// It follows the ticket, not the row, so a refresh that reorders rows does not move it
// onto another ticket.
type dashboard struct {
	rows     []api.TicketSummary // grouped, then sorted
	groups   []ticketgroups.Group
	folds    Folds
	now      func() time.Time
	selected string       // ticket id, empty when the cursor is on a header
	header   domain.Phase // the group whose header the cursor is on
	top      int          // first visible line
}

// item is one visible line of the table: a group header or a ticket.
type item struct {
	header domain.Phase
	ticket string
}

// rank is the sort key: the most urgent need, or beyond every need when there is none.
func rank(t api.TicketSummary) Need {
	if n := Needs(t); len(n) > 0 {
		return n[0]
	}
	return NeedGate + 1
}

// items are the lines showing now: every header, and the tickets of the open groups.
func (d *dashboard) items() []item {
	var out []item
	for _, g := range d.groups {
		out = append(out, item{header: g.Phase})
		if d.folds.folded(g.Phase) {
			continue
		}
		for _, t := range g.Tickets {
			out = append(out, item{ticket: t.ID})
		}
	}
	return out
}

// cursor is where the cursor is in items, or -1.
func (d *dashboard) cursor(items []item) int {
	for i, it := range items {
		if d.selected != "" && it.ticket == d.selected || d.selected == "" && d.header != "" && it.header == d.header {
			return i
		}
	}
	return -1
}

func (d *dashboard) put(it item) {
	d.selected, d.header = it.ticket, it.header
}

// set replaces the fleet, keeping the cursor on the same ticket (or its header when its
// group is folded). If that ticket is gone, the cursor falls to the line now at its old
// position (or the last); with no cursor yet it starts on the first ticket.
func (d *dashboard) set(tickets []api.TicketSummary) {
	if d.folds == nil {
		d.folds = Folds{}
	}
	old := d.items()
	oldIndex := d.cursor(old)
	prev := d.selected
	rows := append([]api.TicketSummary(nil), tickets...)
	sort.SliceStable(rows, func(i, j int) bool {
		if ri, rj := rank(rows[i]), rank(rows[j]); ri != rj {
			return ri < rj
		}
		return rows[i].ID < rows[j].ID
	})
	d.groups = ticketgroups.Of(rows)
	d.rows = d.rows[:0:0]
	for _, g := range d.groups {
		d.rows = append(d.rows, g.Tickets...)
	}
	items := d.items()
	if len(items) == 0 {
		d.selected, d.header, d.top = "", "", 0
		return
	}
	if d.cursor(items) >= 0 {
		return
	}
	if prev != "" {
		for _, g := range d.groups {
			for _, t := range g.Tickets {
				if t.ID == prev { // its group is folded
					d.put(item{header: g.Phase})
					return
				}
			}
		}
	}
	if oldIndex < 0 {
		for _, it := range items {
			if it.ticket != "" {
				d.put(it)
				return
			}
		}
		d.put(items[0])
		return
	}
	d.put(items[min(oldIndex, len(items)-1)])
}

// move changes the cursor by delta visible lines, stopping at the first and last.
func (d *dashboard) move(delta int) {
	items := d.items()
	if len(items) == 0 {
		return
	}
	i := max(d.cursor(items), 0)
	d.put(items[min(max(i+delta, 0), len(items)-1)])
}

// first and last go to the first and last visible ticket (a header only when no ticket
// shows), so g then enter opens a ticket whatever is folded.
func (d *dashboard) first() { d.end(false) }
func (d *dashboard) last()  { d.end(true) }

func (d *dashboard) end(last bool) {
	items := d.items()
	if len(items) == 0 {
		return
	}
	pick := items[0]
	if last {
		pick = items[len(items)-1]
	}
	for i := range items {
		it := items[i]
		if last {
			it = items[len(items)-1-i]
		}
		if it.ticket != "" {
			pick = it
			break
		}
	}
	d.put(pick)
}

// current is the selected ticket; false when the cursor is on a header.
func (d *dashboard) current() (api.TicketSummary, bool) {
	if d.selected == "" {
		return api.TicketSummary{}, false
	}
	for _, r := range d.rows {
		if r.ID == d.selected {
			return r, true
		}
	}
	return api.TicketSummary{}, false
}

// cursorGroup is the phase group the cursor is in.
func (d *dashboard) cursorGroup() (domain.Phase, bool) {
	if d.selected == "" {
		return d.header, d.header != ""
	}
	for _, g := range d.groups {
		for _, t := range g.Tickets {
			if t.ID == d.selected {
				return g.Phase, true
			}
		}
	}
	return "", false
}

// toggle folds or unfolds the cursor's group. Folding from a ticket puts the cursor on
// the header, so it never rests on a line that is gone.
func (d *dashboard) toggle() {
	if p, ok := d.cursorGroup(); ok {
		d.folds[p] = !d.folds.folded(p)
		d.put(item{header: p})
	}
}

// foldOthers folds every group but the cursor's, which is opened.
func (d *dashboard) foldOthers() {
	p, ok := d.cursorGroup()
	if !ok {
		return
	}
	for _, g := range d.groups {
		d.folds[g.Phase] = g.Phase != p
	}
}

func (d *dashboard) openAll() {
	for _, g := range d.groups {
		d.folds[g.Phase] = false
	}
}

// view draws the table in a w x h box: a column header line, as many lines as fit with
// the cursor always visible, then the vessel's totals.
func (d *dashboard) view(w, h int) string {
	if len(d.rows) == 0 {
		return "no tickets yet — create one with: rig new"
	}
	cells := map[string][6]string{}
	widths := [6]int{len("TICKET"), len("PHASE"), len("AGENTS"), len("PORTS"), len("COST"), len("NEEDS")}
	for _, t := range d.rows {
		c := [6]string{t.ID, phaseCell(t), agentsCell(t), portsCell(t), costCell(t), needsCell(t)}
		cells[t.ID] = c
		for i, s := range c {
			widths[i] = max(widths[i], len([]rune(s)))
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

	now := time.Now
	if d.now != nil {
		now = d.now
	}
	footer := ticketgroups.Footer(ticketgroups.TotalsOf(d.rows, now()))
	pane := d.summaryPane(w, h)
	visible := h - 2 - len(pane)
	if visible < 1 {
		visible = 1
	}
	items := d.items()
	i := max(d.cursor(items), 0)
	if i < d.top {
		d.top = i
	}
	if i >= d.top+visible {
		d.top = i - visible + 1
	}
	d.top = min(max(d.top, 0), max(len(items)-visible, 0))
	groups := map[domain.Phase]ticketgroups.Group{}
	for _, g := range d.groups {
		groups[g.Phase] = g
	}
	byID := map[string]api.TicketSummary{}
	for _, t := range d.rows {
		byID[t.ID] = t
	}
	for r := d.top; r < len(items) && r < d.top+visible; r++ {
		it := items[r]
		if it.ticket == "" {
			prefix := "  "
			if r == i {
				prefix = "> "
			}
			out = append(out, ansi.Truncate(prefix+ticketgroups.Header(groups[it.header], d.folds.folded(it.header)), w, "…"))
			continue
		}
		t := byID[it.ticket]
		title := t.Title
		if t.Error != "" {
			title = strings.TrimSpace(title + "  ! " + t.Error)
		}
		out = append(out, line(r == i, cells[it.ticket], title))
	}
	out = append(out, ansi.Truncate(footer, w, "…"))
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
		switch n {
		case NeedGate:
			m = fmt.Sprintf("%s %s", m, t.PendingGate)
		case NeedRequest:
			m = fmt.Sprintf("%s %s (%s)", m, t.PhaseRequest.To, t.PhaseRequest.Service)
		}
		parts = append(parts, m)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}
