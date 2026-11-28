package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/pricing"
	"github.com/charlesrobsampson/rig/internal/usage"
)

// usageReport is `rig usage --json`: docs/formats.md §16.
type usageReport struct {
	Since       string           `json:"since,omitempty"`
	Prices      usagePrices      `json:"prices"`
	Directories []usageDirectory `json:"directories"`
	Totals      usageTotals      `json:"totals"`
}

type usagePrices struct {
	Taken  string `json:"taken"`
	Source string `json:"source"`
}

type usageTotals struct {
	Requests     int          `json:"requests"`
	Tokens       usage.Tokens `json:"tokens"`
	CostUSD      float64      `json:"cost_usd"`
	AgentSeconds int64        `json:"agent_seconds"`
	Unpriced     []string     `json:"unpriced"`
}

type usageDirectory struct {
	Dir      string            `json:"dir"`
	Sessions []usageSessionRow `json:"sessions"`
	usageSession
}

type usageSession struct {
	File         string       `json:"file,omitempty"`
	First        *time.Time   `json:"first,omitempty"`
	Last         *time.Time   `json:"last,omitempty"`
	Requests     int          `json:"requests"`
	Tokens       usage.Tokens `json:"tokens"`
	Models       []string     `json:"models"`
	CostUSD      float64      `json:"cost_usd"`
	Unpriced     []string     `json:"unpriced"`
	AgentSeconds int64        `json:"agent_seconds"`
}

// usageSessionRow is a session: the totals plus where each request ran.
type usageSessionRow struct {
	usageSession
	CWDs []usageCWD `json:"cwds"`
}

type usageCWD struct {
	CWD      string  `json:"cwd"`
	Requests int     `json:"requests"`
	CostUSD  float64 `json:"cost_usd"`
}

func newUsageCmd(a *app) *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "usage <dir>... [--since <time>]",
		Short: "What Claude Code sessions in any directory used, priced from rig's price table",
		Long: `Read the Claude Code transcripts under each directory (its projects/<folder>/*.jsonl,
sub-agent files included; for a Claude Code configuration directory such as ~/.claude)
and print, per directory and per session (one transcript file): first and last time,
requests, tokens (input, cache write, cache read, output), models, cost in dollars and
the time the agent spent working. Each request counts once, as in rig stats. It needs no
daemon, and works for sessions that belong to no ticket.

Cost is worked out from the tokens with the price table built into rig (prices.yaml, with
the date the prices were taken), not from what Claude Code reports, so rig stats can
differ. A model the table does not list is counted in tokens, named as unpriced, and left
out of the cost, which then shows a + as a lower bound.

--since counts only entries at or after the time, an RFC 3339 time or a date
(2026-11-01, local midnight): for a month's spend. A directory that has no transcripts is
an error naming it.`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var from time.Time
			if since != "" {
				var err error
				if from, err = parseSince(since); err != nil {
					return usageError(err)
				}
			}
			prices, err := pricing.Default()
			if err != nil {
				return err
			}
			rep := usageReport{Prices: usagePrices{Taken: prices.Taken, Source: prices.Source}, Directories: []usageDirectory{}}
			if !from.IsZero() {
				rep.Since = from.UTC().Format(time.RFC3339)
			}
			unpriced := map[string]bool{}
			for _, dir := range args {
				d, err := usage.ReadDir(dir, from, prices)
				if err != nil {
					return usageError(err)
				}
				ud := usageDirectory{Dir: dir, Sessions: []usageSessionRow{}, usageSession: asSession("", d.Total, d.CostUSD, d.Unpriced)}
				for _, s := range d.Sessions {
					us := usageSessionRow{usageSession: asSession(s.File, s.Transcript, s.CostUSD, s.Unpriced), CWDs: []usageCWD{}}
					for _, c := range s.CWDs {
						us.CWDs = append(us.CWDs, usageCWD{CWD: c.CWD, Requests: c.Requests, CostUSD: c.CostUSD})
					}
					ud.Sessions = append(ud.Sessions, us)
				}
				rep.Directories = append(rep.Directories, ud)
				rep.Totals.Requests += d.Total.Requests
				rep.Totals.Tokens.Add(d.Total.Tokens)
				rep.Totals.CostUSD += d.CostUSD
				rep.Totals.AgentSeconds += d.Total.WorkSeconds
				for _, m := range d.Unpriced {
					unpriced[m] = true
				}
			}
			rep.Totals.Unpriced = []string{}
			for m := range unpriced {
				rep.Totals.Unpriced = append(rep.Totals.Unpriced, m)
			}
			sort.Strings(rep.Totals.Unpriced)
			if a.json {
				return printJSON(cmd.OutOrStdout(), rep)
			}
			writeUsage(cmd.OutOrStdout(), rep)
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "count only entries at or after this time (RFC 3339, or a date like 2026-11-01)")
	return cmd
}

func asSession(file string, t usage.Transcript, cost float64, unpriced []string) usageSession {
	s := usageSession{File: file, Requests: t.Requests, Tokens: t.Tokens, Models: t.Models, CostUSD: cost, Unpriced: unpriced, AgentSeconds: t.WorkSeconds}
	if s.Models == nil {
		s.Models = []string{}
	}
	if s.Unpriced == nil {
		s.Unpriced = []string{}
	}
	if !t.First.IsZero() {
		f, l := t.First.UTC(), t.Last.UTC()
		s.First, s.Last = &f, &l
	}
	return s
}

func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q is not an RFC 3339 time or a date like 2026-11-01", s)
}

func usageTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format("2006-01-02 15:04")
}

// usageCWDs is the first working directory of a session, and +N for N more.
func usageCWDs(cwds []usageCWD) string {
	if len(cwds) == 0 {
		return "-"
	}
	out := cwds[0].CWD
	if out == "" {
		out = "-"
	}
	if len(cwds) > 1 {
		out += fmt.Sprintf(" +%d", len(cwds)-1)
	}
	return out
}

func usageCost(cost float64, unpriced []string) string {
	if len(unpriced) > 0 {
		return fmt.Sprintf("$%.2f+", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

func writeUsage(w io.Writer, rep usageReport) {
	row := func(tw io.Writer, name string, cwds []usageCWD, s usageSession) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, usageCWDs(cwds), usageTime(s.First), usageTime(s.Last), s.Requests,
			tokens(s.Tokens.Input), tokens(s.Tokens.CacheWrite), tokens(s.Tokens.CacheRead), tokens(s.Tokens.Output),
			usageCost(s.CostUSD, s.Unpriced), duration(s.AgentSeconds), strings.Join(s.Models, ","))
	}
	for _, d := range rep.Directories {
		fmt.Fprintln(w, d.Dir)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SESSION\tCWD\tFIRST (UTC)\tLAST (UTC)\tREQUESTS\tINPUT\tCACHE WRITE\tCACHE READ\tOUTPUT\tCOST\tTIME\tMODELS")
		for _, s := range d.Sessions {
			row(tw, s.File, s.CWDs, s.usageSession)
		}
		row(tw, "TOTAL", nil, d.usageSession)
		tw.Flush()
		if len(d.Unpriced) > 0 {
			fmt.Fprintf(w, "unpriced (not in the price table, tokens counted, cost left out): %s\n", strings.Join(d.Unpriced, ", "))
		}
		fmt.Fprintln(w)
	}
	if len(rep.Directories) > 1 {
		t := rep.Totals
		fmt.Fprintf(w, "ALL  %d requests  in %s  out %s  %s  %s\n", t.Requests, tokens(t.Tokens.In()), tokens(t.Tokens.Output), usageCost(t.CostUSD, t.Unpriced), duration(t.AgentSeconds))
	}
	since := ""
	if rep.Since != "" {
		since = ", entries from " + rep.Since
	}
	fmt.Fprintf(w, "prices taken %s%s\n", rep.Prices.Taken, since)
}
