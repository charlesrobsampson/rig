package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newReviewerCmd(a *app) *cobra.Command {
	var start, pass, stop bool
	var reason string
	cmd := &cobra.Command{
		Use:   "reviewer <ticket-id>",
		Short: "Show or steer a ticket's review loop",
		Long: `A ticket with a review in its ticket.yaml has a reviewer agent: rig runs rounds
between it and the reviewed service's agent until the review passes, and the ticket
cannot leave implement before that.

With no flag, shows the loop: its status, each round's findings and the coder's answers,
and what it has cost. --start starts a round now (after a stop, a new loop). --pass and
--stop record a person's decision, with --reason: pass lets the ticket move on, stop ends
the loop until someone starts it again.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			chosen := 0
			for _, f := range []bool{start, pass, stop} {
				if f {
					chosen++
				}
			}
			switch {
			case chosen > 1:
				return usageError(errors.New("choose one of --start, --pass and --stop"))
			case (pass || stop) && strings.TrimSpace(reason) == "":
				return usageError(errors.New("--pass and --stop need --reason"))
			case reason != "" && !pass && !stop:
				return usageError(errors.New("--reason goes with --pass or --stop"))
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.ReviewerStatus
			switch {
			case start:
				err = c.Call(cmd.Context(), api.MethodReviewerStart, api.ReviewerParams{Ticket: args[0]}, &res)
			case pass, stop:
				outcome := map[bool]string{true: "pass", false: "stop"}[pass]
				err = c.Call(cmd.Context(), api.MethodReviewerDecide, api.ReviewerDecideParams{Ticket: args[0], Outcome: outcome, Reason: reason}, &res)
			default:
				err = c.Call(cmd.Context(), api.MethodReviewerStatus, api.ReviewerParams{Ticket: args[0]}, &res)
			}
			if err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			printReviewer(cmd.OutOrStdout(), args[0], res)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&start, "start", false, "start a review round now")
	f.BoolVar(&pass, "pass", false, "pass the review, so the ticket can move on (needs --reason)")
	f.BoolVar(&stop, "stop", false, "stop the review loop for a person to look at (needs --reason)")
	f.StringVar(&reason, "reason", "", "why, for --pass and --stop")
	return cmd
}

func printReviewer(w io.Writer, ticket string, r api.ReviewerStatus) {
	if r.Status == "" {
		fmt.Fprintf(w, "review of %s: no round yet (at most %d rounds)\n", ticket, r.MaxRounds)
	} else {
		fmt.Fprintf(w, "review of %s: %s, round %d of at most %d", ticket, r.Status, r.Round, r.MaxRounds)
		if r.Commit != "" {
			fmt.Fprintf(w, " (commit %s)", shortSHA(r.Commit))
		}
		fmt.Fprintln(w)
		if r.Reason != "" {
			fmt.Fprintf(w, "  %s: %s\n", r.By, r.Reason)
		}
	}
	budget := ""
	if r.BudgetUSD > 0 {
		budget = fmt.Sprintf(" of $%.2f", r.BudgetUSD)
	}
	fmt.Fprintf(w, "spent $%.2f%s this loop (coder $%.2f, reviewer $%.2f in all)\n", r.SpentUSD, budget, r.CoderUSD, r.ReviewerUSD)
	for _, round := range r.Rounds {
		verdict := round.Verdict
		if verdict == "" {
			verdict = "no verdict yet"
		}
		fmt.Fprintf(w, "\nround %d on %s: %s\n", round.Round, shortSHA(round.Commit), verdict)
		for _, f := range round.Findings {
			where := f.File
			if f.Line > 0 {
				where += fmt.Sprintf(":%d", f.Line)
			}
			answer := "no answer"
			if f.Reply != nil {
				answer = f.Reply.Status
				if f.Reply.Commit != "" {
					answer += " in " + shortSHA(f.Reply.Commit)
				}
				if f.Reply.Reason != "" {
					answer += ": " + f.Reply.Reason
				}
			}
			fmt.Fprintf(w, "  %s %-10s %s %s\n    -> %s\n", f.ID, f.Severity, where, f.What, answer)
		}
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
