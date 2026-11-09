package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func newListCmd(a *app) *cobra.Command {
	var summaries bool
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
			writeTicketTable(cmd.OutOrStdout(), res.Tickets)
			if summaries {
				writeSummaries(cmd.OutOrStdout(), res.Tickets)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&summaries, "summaries", false, "also show each ticket's language-model summary")
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

func writeTicketTable(w io.Writer, tickets []api.TicketSummary) {
	if len(tickets) == 0 {
		fmt.Fprintln(w, "no tickets")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPHASE\tSERVICES\tPORTS\tTITLE")
	for _, t := range tickets {
		phase := string(t.Phase)
		if phase == "" {
			phase = "-"
		}
		if t.ContractPending {
			phase += " (contract change pending)"
		}
		if t.ContractViolation {
			phase += " (contract edited outside rig)"
		}
		if len(t.Drift) > 0 {
			phase += fmt.Sprintf(" (drift: %d, see rig reconcile)", len(t.Drift))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, phase, serviceCell(t.Services), portsCell(t.Services), titleCell(t))
	}
	tw.Flush()
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
