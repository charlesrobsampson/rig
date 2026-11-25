package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newStatsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "stats [ticket-id...]",
		Short: "What each ticket used: cost and tokens per agent, time per phase, waits, rounds, change size",
		Long: `Show what rig measured, per ticket and added up for the vessel: cost and tokens
(fresh input, cache writes, cache reads, output) for each agent with its models, time
in each phase, time waiting for a person at each gate, review rounds, and the size of
each service's change. One ticket named: its full breakdown. --json gives everything.

Tokens come from each agent's own Claude Code transcripts in the ticket directory; a
host agent on your own login has none there, so its tokens show as 0.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.StatsResult
			if err := c.Call(cmd.Context(), api.MethodTicketStats, api.StatsParams{IDs: args}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if len(args) == 1 && len(res.Tickets) == 1 {
				writeTicketStats(cmd.OutOrStdout(), res.Tickets[0])
				return nil
			}
			writeStatsTable(cmd.OutOrStdout(), res)
			return nil
		},
	}
}

func in(t api.TokenCount) int64 { return t.Input + t.CacheWrite + t.CacheRead }

func changeCell(changes map[string]api.ChangeStats) string {
	if len(changes) == 0 {
		return "-"
	}
	var added, deleted int
	for _, c := range changes {
		added, deleted = added+c.Added, deleted+c.Deleted
	}
	return fmt.Sprintf("+%d -%d", added, deleted)
}

func writeStatsTable(w io.Writer, res api.StatsResult) {
	if len(res.Tickets) == 0 {
		fmt.Fprintln(w, "no tickets")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TICKET\tPHASE\tCOST\tIN\tOUT\tROUNDS\tCHANGE\tTIME\tWAITING ON PEOPLE")
	for _, t := range res.Tickets {
		if t.Error != "" {
			fmt.Fprintf(tw, "%s\t! %s\t\t\t\t\t\t\t\n", t.ID, t.Error)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t$%.2f\t%s\t%s\t%d\t%s\t%s\t%s\n", t.ID, t.Phase, t.CostUSD, tokens(in(t.Tokens)), tokens(t.Tokens.Output),
			t.ReviewRounds, changeCell(t.Changes), duration(t.ElapsedSeconds), duration(t.WaitingOnPeopleSeconds))
	}
	tot := res.Totals
	fmt.Fprintf(tw, "TOTAL\t%d of %d done\t$%.2f\t%s\t%s\t%d\t+%d -%d\t%s\t\n", tot.Done, tot.Tickets, tot.CostUSD, tokens(in(tot.Tokens)), tokens(tot.Tokens.Output),
		tot.ReviewRounds, tot.Added, tot.Deleted, duration(tot.ElapsedSeconds))
	tw.Flush()
}

func writeTicketStats(w io.Writer, t api.TicketStats) {
	fmt.Fprintf(w, "%s: %s (%s)\n\n", t.ID, t.Title, t.Phase)
	fmt.Fprintf(w, "cost    $%.2f\n", t.CostUSD)
	fmt.Fprintf(w, "tokens  in %s (fresh %s, cache write %s, cache read %s), out %s\n", tokens(in(t.Tokens)), tokens(t.Tokens.Input), tokens(t.Tokens.CacheWrite), tokens(t.Tokens.CacheRead), tokens(t.Tokens.Output))
	var phases []string
	for _, p := range t.Phases {
		phases = append(phases, fmt.Sprintf("%s %s", p.Phase, duration(p.Seconds)))
	}
	fmt.Fprintf(w, "time    %s (%s)\n", duration(t.ElapsedSeconds), strings.Join(phases, ", "))
	if len(t.Gates) > 0 {
		var gates []string
		for _, g := range t.Gates {
			gates = append(gates, fmt.Sprintf("%s %s", g.Gate, duration(g.Seconds)))
		}
		fmt.Fprintf(w, "waiting %s on people (%s)\n", duration(t.WaitingOnPeopleSeconds), strings.Join(gates, ", "))
	}
	if t.ReviewRounds > 0 {
		fmt.Fprintf(w, "review  %s after %d round(s)\n", t.ReviewStatus, t.ReviewRounds)
	}
	if len(t.Changes) > 0 {
		var names []string
		for name := range t.Changes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			c := t.Changes[name]
			fmt.Fprintf(w, "change  %s: %d files, +%d -%d\n", name, c.Files, c.Added, c.Deleted)
		}
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tSERVICE\tCOST\tIN\tOUT\tREQUESTS\tMODELS")
	for _, a := range t.Agents {
		fmt.Fprintf(tw, "%s\t%s\t$%.2f\t%s\t%s\t%d\t%s\n", a.Role, a.Service, a.CostUSD, tokens(in(a.Tokens)), tokens(a.Tokens.Output), a.Requests, strings.Join(a.Models, ", "))
	}
	tw.Flush()
}
