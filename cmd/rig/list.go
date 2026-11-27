package main

import (
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"io"
	"slices"
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
	header, rows, _ := ticketRows(tickets, tickets, now, width)
	writeFitted(w, header, rows, width, []int{len(header) - 1, 1})
}

// writeGroupedTable is the table with a row for each phase's group and a last row for
// the whole vessel (ticketgroups says what is in them), so their totals sit under the
// columns they total. Done tickets are one row unless allDone.
func writeGroupedTable(w io.Writer, tickets []api.TicketSummary, now time.Time, width int, allDone bool) {
	if len(tickets) == 0 {
		fmt.Fprintln(w, "no tickets")
		return
	}
	var shown []api.TicketSummary
	var summaryRows []ticketgroups.Row
	var at []int // the ticket row each group header comes before
	for _, g := range ticketgroups.Of(tickets) {
		folded := g.Phase == domain.PhaseDone && !allDone
		summaryRows = append(summaryRows, ticketgroups.GroupRow(g, folded, now))
		at = append(at, len(shown))
		if !folded {
			shown = append(shown, g.Tickets...)
		}
	}
	header, rows, hasPorts := ticketRows(shown, tickets, now, width)
	var out [][]string
	next := 0
	for i, r := range rows {
		for next < len(at) && at[next] == i {
			out = append(out, summaryCells(summaryRows[next], hasPorts))
			next++
		}
		out = append(out, r)
	}
	for ; next < len(at); next++ {
		out = append(out, summaryCells(summaryRows[next], hasPorts))
	}
	out = append(out, nil, summaryCells(ticketgroups.TotalRow(tickets, now), hasPorts))
	writeFitted(w, header, out, width, []int{len(header) - 1, 1})
}

// summaryCells puts a group or footer row under the table's columns: ID PHASE SERVICES
// COST TIME AGENT [PORTS] TITLE. PHASE is blank, the label says it.
func summaryCells(r ticketgroups.Row, hasPorts bool) []string {
	cells := []string{r.Label, "", r.Services, r.Cost, r.Time, r.Agent}
	if hasPorts {
		cells = append(cells, r.Ports)
	}
	return append(cells, r.Title)
}

// ticketRows is the header and a row per ticket; overlaps are found among all, so a
// ticket left out of the rows still counts.
func ticketRows(tickets, all []api.TicketSummary, now time.Time, width int) (header []string, rows [][]string, hasPorts bool) {
	header = []string{"ID", "PHASE", "SERVICES", "COST", "TIME", "AGENT", "PORTS", "TITLE"}
	rows = make([][]string, len(tickets))
	anyPorts := false
	overlaps := overlapsOf(all)
	for i, t := range tickets {
		ports := ticketgroups.PortsText(t.Services)
		anyPorts = anyPorts || ports != "-"
		rows[i] = []string{t.ID, phaseCell(t, overlaps[t.ID]), ticketgroups.ServicesText(t.Services), ticketgroups.CostText(ticketgroups.CostOf(t)), ticketgroups.ElapsedText(t, now), ticketgroups.AgentText(t.AgentSeconds), ports, ticketgroups.TitleText(t)}
	}
	hasPorts = true
	if width > 0 && !anyPorts {
		hasPorts = false
		header = slices.Delete(header, 6, 7)
		for i := range rows {
			rows[i] = slices.Delete(rows[i], 6, 7)
		}
	}
	return header, rows, hasPorts
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

// writeFitted prints a table with two spaces between columns. With width > 0 it
// shortens the shrinkable columns, in order, until each line fits. A nil row is a blank
// line. Every row, group headers and footer included, counts for the column widths.
func writeFitted(w io.Writer, header []string, rows [][]string, width int, shrinkable []int) {
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
	for _, r := range all {
		if r == nil {
			fmt.Fprintln(w)
			continue
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
