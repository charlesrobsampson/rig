package main

import (
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/ticketgroups"
)

func newListCmd(a *app) *cobra.Command {
	var summaries, all, wide, group, allDone bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tickets with their phase, services, and ports",
		Long: `List tickets with their phase, services and ports.

--summaries also prints each ticket's language-model summary, when summarizer.provider
is set in the config (ollama or claude) and one has been made. A summary is advisory: it
is always printed with the real diff stats it was made from, and the backend that wrote
it is named, marked "remote" when the content left this machine.

AGENT is the time the ticket's agents spent working (what rig stats reports); TIME is the
ticket's age, which overlaps other tickets and includes waiting.

--group sorts the tickets under one line per phase, in lifecycle order, each with its
ticket count, cost and agent time, and a line totalling the vessel. Done tickets are left
as that one line unless --all-done.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if allDone && !group {
				return usageError(fmt.Errorf("--all-done goes with --group"))
			}
			if all && group {
				return usageError(fmt.Errorf("--group lists one vessel; it does not go with --all"))
			}
			if all {
				return a.listAll(cmd)
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TicketListResult
			if err := c.Call(cmd.Context(), api.MethodTicketList, nil, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			width := a.termWidth()
			if wide {
				width = 0
			}
			if group {
				writeGroupedTable(cmd.OutOrStdout(), res.Tickets, a.now(), width, allDone)
			} else {
				writeTicketTable(cmd.OutOrStdout(), res.Tickets, a.now(), width)
			}
			if summaries {
				writeSummaries(cmd.OutOrStdout(), res.Tickets)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&summaries, "summaries", false, "also show each ticket's language-model summary")
	cmd.Flags().BoolVar(&group, "group", false, "group the tickets by phase, each group under a line with its count, cost and agent time")
	cmd.Flags().BoolVar(&allDone, "all-done", false, "with --group, list the done tickets too instead of one line")
	cmd.Flags().BoolVar(&wide, "wide", false, "do not shorten anything to fit the terminal")
	cmd.Flags().BoolVar(&all, "all", false, "the tickets of every home whose daemon runs (rig homes), starting none")
	return cmd
}

// writeSummaries prints each ticket's summary under the table, labelled so nobody takes
// it for fact: the backend, whether it is remote, and the raw diff stats beside it.
func writeSummaries(w io.Writer, tickets []api.TicketSummary) {
	shown := 0
	for _, t := range tickets {
		s := t.AISummary
		if s == nil {
			continue
		}
		if shown == 0 {
			fmt.Fprintln(w)
		}
		shown++
		backend := s.Backend
		if s.Model != "" {
			backend += " " + s.Model
		}
		if s.Remote {
			backend += ", REMOTE: the ticket's content was sent to it"
		}
		stats := fmt.Sprintf("%d files +%d -%d", s.Files, s.Added, s.Deleted)
		at := ""
		if s.At != nil {
			at = ", " + s.At.Local().Format("15:04")
		}
		fmt.Fprintf(w, "%s  summary (advisory; %s; %s%s)\n", t.ID, backend, stats, at)
		for _, line := range strings.Split(strings.TrimSpace(s.Text), "\n") {
			if line != "" {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
		if s.Error != "" {
			note := "refresh failed"
			if s.Text != "" {
				note = "the summary above is out of date; refresh failed"
			}
			fmt.Fprintf(w, "    (%s: %s)\n", note, s.Error)
		}
	}
	if shown == 0 {
		fmt.Fprintln(w, "\nno summaries (set summarizer.provider in the config to ollama or claude)")
	}
}

// writeTicketTable prints one line per ticket. width > 0 is the terminal's width: the
// title (then the phase notes) is shortened to fit it, and an all-empty PORTS column is
// left out. width 0 (a pipe, a script, --wide) prints everything.
func writeTicketTable(w io.Writer, tickets []api.TicketSummary, now time.Time, width int) {
	if len(tickets) == 0 {
		fmt.Fprintln(w, "no tickets")
		return
	}
	header, rows := ticketRows(tickets, tickets, now, width)
	writeFitted(w, header, rows, width, []int{len(header) - 1, 1}, nil)
}

// writeGroupedTable is the table under one line per phase (ticketgroups, as the TUI
// draws it) and a totals line. Done tickets are one line unless allDone.
func writeGroupedTable(w io.Writer, tickets []api.TicketSummary, now time.Time, width int, allDone bool) {
	if len(tickets) == 0 {
		fmt.Fprintln(w, "no tickets")
		return
	}
	var shown []api.TicketSummary
	var breaks []sectionBreak
	groups := ticketgroups.Of(tickets)
	for _, g := range groups {
		folded := g.Phase == domain.PhaseDone && !allDone
		breaks = append(breaks, sectionBreak{len(shown), ticketgroups.Header(g, folded)})
		if !folded {
			shown = append(shown, g.Tickets...)
		}
	}
	header, rows := ticketRows(shown, tickets, now, width)
	writeFitted(w, header, rows, width, []int{len(header) - 1, 1}, breaks)
	fmt.Fprintf(w, "\n%s\n", ticketgroups.Footer(ticketgroups.TotalsOf(tickets, now)))
}

// ticketRows is the header and a row per ticket; overlaps are found among all, so a
// ticket left out of the rows still counts.
func ticketRows(tickets, all []api.TicketSummary, now time.Time, width int) ([]string, [][]string) {
	header := []string{"ID", "PHASE", "SERVICES", "COST", "TIME", "AGENT", "PORTS", "TITLE"}
	rows := make([][]string, len(tickets))
	anyPorts := false
	overlaps := overlapsOf(all)
	for i, t := range tickets {
		ports := portsCell(t.Services)
		anyPorts = anyPorts || ports != "-"
		rows[i] = []string{t.ID, phaseCell(t, overlaps[t.ID]), serviceCell(t.Services), costCell(t.CostUSD), elapsedCell(t, now), agentCell(t), ports, titleCell(t)}
	}
	if width > 0 && !anyPorts {
		header = slices.Delete(header, 6, 7)
		for i := range rows {
			rows[i] = slices.Delete(rows[i], 6, 7)
		}
	}
	return header, rows
}

func agentCell(t api.TicketSummary) string {
	if t.AgentSeconds == 0 {
		return "-"
	}
	return duration(t.AgentSeconds)
}

// phaseCell is the phase and what is going on in it, as short notes.
func phaseCell(t api.TicketSummary, overlaps []string) string {
	notes := []string{string(t.Phase)}
	if t.Phase == "" {
		notes[0] = "-"
	}
	if q := t.Question; q != nil {
		notes = append(notes, fmt.Sprintf("%s asks you", q.Service))
	}
	if r := t.PhaseRequest; r != nil {
		notes = append(notes, fmt.Sprintf("%s asks for %s", r.Service, r.To))
	}
	if r := t.Review; r != nil {
		if r.Status == domain.ReviewAwaitingReady {
			notes = append(notes, fmt.Sprintf("review waits for ready r%d", r.Round))
		} else if r.Stalled != nil {
			who := r.Stalled.Service
			if who == domain.ReviewerService {
				who = "reviewer"
			}
			notes = append(notes, fmt.Sprintf("review stalled (%s idle)", who))
		} else if r.Stale {
			notes = append(notes, fmt.Sprintf("review %s r%d (new commits)", r.Status, r.Round))
		} else {
			notes = append(notes, fmt.Sprintf("review %s r%d", r.Status, r.Round))
		}
	}
	if t.ContractPending {
		notes = append(notes, "contract change pending")
	}
	if t.ContractViolation {
		notes = append(notes, "contract edited outside rig")
	}
	for _, b := range t.Base {
		note := fmt.Sprintf("behind %s %d", b.Base, b.Behind)
		if len(b.Conflicts) > 0 {
			note += fmt.Sprintf(", conflicts %d", len(b.Conflicts))
		}
		notes = append(notes, note)
	}
	for _, o := range overlaps {
		notes = append(notes, "overlaps "+o)
	}
	if len(t.Drift) > 0 {
		notes = append(notes, fmt.Sprintf("drift %d", len(t.Drift)))
	}
	return strings.Join(notes, " · ")
}

// sectionBreak is a line printed before row `at`.
type sectionBreak struct {
	at   int
	line string
}

// writeFitted prints a table with two spaces between columns. With width > 0 it
// shortens the shrinkable columns, in order, until each line fits. Break lines are
// printed whole, before the row they name, and count for nothing in the column widths.
func writeFitted(w io.Writer, header []string, rows [][]string, width int, shrinkable []int, breaks []sectionBreak) {
	const gap, least = 2, 12
	all := append([][]string{header}, rows...)
	widths := make([]int, len(header))
	for _, r := range all {
		for c, cell := range r {
			widths[c] = max(widths[c], ansi.StringWidth(cell))
		}
	}
	total := func() int {
		sum := gap * (len(widths) - 1)
		for _, x := range widths {
			sum += x
		}
		return sum
	}
	if width > 0 {
		for _, c := range shrinkable {
			if over := total() - width; over > 0 {
				widths[c] = max(widths[c]-over, min(least, widths[c]))
			}
		}
	}
	for i, r := range all {
		for i > 0 && len(breaks) > 0 && breaks[0].at <= i-1 {
			fmt.Fprintln(w, breaks[0].line)
			breaks = breaks[1:]
		}
		var b strings.Builder
		for c, cell := range r {
			cell = ansi.Truncate(cell, widths[c], "…")
			b.WriteString(cell)
			if c < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[c]-ansi.StringWidth(cell)+gap))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	for _, br := range breaks {
		fmt.Fprintln(w, br.line)
	}
}

func serviceCell(services []api.ServiceSummary) string {
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

func costCell(usd *float64) string {
	if usd == nil {
		return "-"
	}
	return fmt.Sprintf("$%.2f", *usd)
}

// elapsedCell is the ticket's age: from creation to done, or to now while it is open.
func elapsedCell(t api.TicketSummary, now time.Time) string {
	if t.CreatedAt == nil {
		return "-"
	}
	end := now
	if t.DoneAt != nil {
		end = *t.DoneAt
	}
	return duration(int64(end.Sub(*t.CreatedAt).Seconds()))
}

func duration(seconds int64) string { return ticketgroups.Duration(seconds) }

// tokens is a short human form: 950, 24k, 1.9M.
func tokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1e6)
}

func portsCell(services []api.ServiceSummary) string {
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

func titleCell(t api.TicketSummary) string {
	switch {
	case t.Error == "":
		return t.Title
	case t.Title == "":
		return "! " + t.Error
	}
	return t.Title + "  ! " + t.Error
}

// overlapsOf finds, for each ticket, the other tickets in the list that have changed a
// file it has changed too, so the conflict is seen before it happens. A file counts
// when the same service name touches the same path (a vessel's services are its
// repositories); tickets with nothing measured, done ones among them, take no part.
func overlapsOf(tickets []api.TicketSummary) map[string][]string {
	type key struct{ service, path string }
	by := map[key][]string{}
	for _, t := range tickets {
		if t.Phase == domain.PhaseDone {
			continue
		}
		for _, ti := range t.Touched {
			for _, f := range ti.Files {
				k := key{ti.Service, f}
				if !slices.Contains(by[k], t.ID) {
					by[k] = append(by[k], t.ID)
				}
			}
		}
	}
	out := map[string][]string{}
	for _, ids := range by {
		for _, id := range ids {
			for _, other := range ids {
				if other != id && !slices.Contains(out[id], other) {
					out[id] = append(out[id], other)
				}
			}
		}
	}
	for id := range out {
		slices.Sort(out[id])
	}
	return out
}
