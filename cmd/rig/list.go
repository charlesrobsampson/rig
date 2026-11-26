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
)

func newListCmd(a *app) *cobra.Command {
	var summaries, all, wide bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tickets with their phase, services, and ports",
		Long: `List tickets with their phase, services and ports.

--summaries also prints each ticket's language-model summary, when summarizer.provider
is set in the config (ollama or claude) and one has been made. A summary is advisory: it
is always printed with the real diff stats it was made from, and the backend that wrote
it is named, marked "remote" when the content left this machine.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			writeTicketTable(cmd.OutOrStdout(), res.Tickets, a.now(), width)
			if summaries {
				writeSummaries(cmd.OutOrStdout(), res.Tickets)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&summaries, "summaries", false, "also show each ticket's language-model summary")
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
	header := []string{"ID", "PHASE", "SERVICES", "COST", "TIME", "PORTS", "TITLE"}
	rows := make([][]string, len(tickets))
	anyPorts := false
	overlaps := overlapsOf(tickets)
	for i, t := range tickets {
		ports := portsCell(t.Services)
		anyPorts = anyPorts || ports != "-"
		rows[i] = []string{t.ID, phaseCell(t, overlaps[t.ID]), serviceCell(t.Services), costCell(t.CostUSD), elapsedCell(t, now), ports, titleCell(t)}
	}
	if width > 0 && !anyPorts {
		header = slices.Delete(header, 5, 6)
		for i := range rows {
			rows[i] = slices.Delete(rows[i], 5, 6)
		}
	}
	writeFitted(w, header, rows, width, []int{len(header) - 1, 1})
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
// shortens the shrinkable columns, in order, until each line fits.
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

// duration is a short human form: 45s, 12m, 1h05m, 3d04h.
func duration(seconds int64) string {
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
