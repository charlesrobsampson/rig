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
	return &cobra.Command{
		Use:   "list",
		Short: "List tickets with their phase, services, and ports",
		Args:  usageArgs(cobra.NoArgs),
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
			return nil
		},
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
