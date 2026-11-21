package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// waitPoll is how often rig wait asks the daemon again.
const waitPoll = 500 * time.Millisecond

type waitResult struct {
	Matched string             `json:"matched"` // record, phase_request, phase or review
	Record  *api.JournalRecord `json:"record,omitempty"`
	Summary api.TicketSummary  `json:"summary"`
}

func newWaitCmd(a *app) *cobra.Command {
	var kind, phase, review string
	var request bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <ticket-id>",
		Short: "Wait until something happens to a ticket",
		Long: `Waits until any of the given things is true, prints it, and exits 0; exits 1 on
--timeout. --record waits for a new journal record of that kind (one left after wait
started); --phase-request for an agent's open request to move the ticket; --phase for
the ticket to be in that phase; --review for its review loop to reach that status
(reviewing, fixing, passed or stopped). States already true count at once.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind == "" && !request && phase == "" && review == "" {
				return usageError(errors.New("say what to wait for: --record, --phase-request, --phase or --review"))
			}
			if phase != "" && !domain.Phase(phase).Valid() {
				return usageError(fmt.Errorf("%q is not a phase", phase))
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			clk := a.deps.clock
			if clk == nil {
				clk = clock.Real()
			}
			since := clk.Now()
			deadline := clk.After(timeout)
			for {
				var t api.TicketGetResult
				if err := c.Call(cmd.Context(), api.MethodTicketGet, api.TicketGetParams{ID: args[0]}, &t); err != nil {
					return err
				}
				res := waitResult{Summary: t.Summary}
				s := t.Summary
				switch {
				case request && s.PhaseRequest != nil:
					res.Matched = "phase_request"
				case phase != "" && string(s.Phase) == phase:
					res.Matched = "phase"
				case review != "" && s.Review != nil && string(s.Review.Status) == review:
					res.Matched = "review"
				}
				if res.Matched == "" && kind != "" {
					var j api.JournalListResult
					if err := c.Call(cmd.Context(), api.MethodJournalList, api.JournalListParams{Ticket: args[0], Kind: kind}, &j); err != nil {
						return err
					}
					for i := range j.Records {
						if !j.Records[i].At.Before(since) {
							res.Matched, res.Record = "record", &j.Records[i]
							break
						}
					}
				}
				if res.Matched != "" {
					if a.json {
						return printJSON(cmd.OutOrStdout(), res)
					}
					out := cmd.OutOrStdout()
					switch res.Matched {
					case "record":
						fmt.Fprintf(out, "%s %s from %s: %s\n", res.Record.At.Local().Format("15:04:05"), res.Record.Kind, res.Record.Service, res.Record.Text)
					case "phase_request":
						fmt.Fprintf(out, "%s asks for %s: %s\n", s.PhaseRequest.Service, s.PhaseRequest.To, s.PhaseRequest.Reason)
					case "phase":
						fmt.Fprintf(out, "phase is %s\n", s.Phase)
					case "review":
						fmt.Fprintf(out, "review %s, round %d\n", s.Review.Status, s.Review.Round)
					}
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-deadline:
					return fmt.Errorf("timed out after %s", timeout)
				case <-clk.After(waitPoll):
				}
			}
		},
	}
	f := cmd.Flags()
	f.StringVar(&kind, "record", "", "a journal record of this kind")
	f.BoolVar(&request, "phase-request", false, "an agent's request to move the ticket")
	f.StringVar(&phase, "phase", "", "the ticket being in this phase")
	f.StringVar(&review, "review", "", "the review loop reaching this status")
	f.DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait")
	return cmd
}
