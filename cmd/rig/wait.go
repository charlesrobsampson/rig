package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/client"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// waitRetry is how long rig wait lets a broken event stream rest before it subscribes again.
const waitRetry = time.Second

type waitResult struct {
	// Ticket is the ticket that matched, as it was named ("t1" or "osmo/t1").
	Ticket  string             `json:"ticket"`
	Matched string             `json:"matched"` // record, phase_request, phase or review
	Record  *api.JournalRecord `json:"record,omitempty"`
	Summary api.TicketSummary  `json:"summary"`
}

func newWaitCmd(a *app) *cobra.Command {
	var kind string
	var phases, reviews []string
	var request, fresh bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <ticket>...",
		Short: "Wait until something happens to a ticket (or to any of several)",
		Long: `Waits until any of the given things is true, prints it, and exits 0; exits 1 on
--timeout. --record waits for a new journal record of that kind (one left after wait
started); --phase-request for an agent's open request to move the ticket; --phase for
the ticket to be in that phase; --review for its review loop to reach that status
(reviewing, fixing, passed or stopped). --phase and --review take several values,
repeated or comma-separated: "--review passed,stopped" ends on either. States already
true count at once, except a phase request with --new, which must be made after the
wait starts (to wait for the next request after answering one).

Several tickets: it ends on the first that matches and names it. A ticket can name its
vessel, so one wait can watch several: rig wait osmo/t14 jybe/j02 --review passed.`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind == "" && !request && len(phases) == 0 && len(reviews) == 0 {
				return usageError(errors.New("say what to wait for: --record, --phase-request, --phase or --review"))
			}
			for _, p := range phases {
				if !domain.Phase(p).Valid() {
					return usageError(fmt.Errorf("%q is not a phase", p))
				}
			}
			for _, r := range reviews {
				if !domain.ReviewStatus(r).Valid() {
					return usageError(fmt.Errorf("%q is not a review status (reviewing, fixing, passed or stopped)", r))
				}
			}
			// Each ticket may be on its own vessel; one connection per vessel for questions
			// and one more for its event stream.
			type target struct {
				label, id string
				c         *client.Client
			}
			type vessel struct {
				on      *app
				ids     []string
				c, subC *client.Client
			}
			var targets []target
			vessels := map[string]*vessel{}
			defer func() {
				for _, v := range vessels {
					v.c.Close()
					v.subC.Close()
				}
			}()
			for _, arg := range args {
				on, id, err := a.ticketRef(arg)
				if err != nil {
					return err
				}
				v, ok := vessels[on.configPath]
				if !ok {
					v = &vessel{on: on}
					if v.c, err = on.connect(cmd.Context(), true); err != nil {
						return err
					}
					vessels[on.configPath] = v
					if v.subC, err = on.connect(cmd.Context(), true); err != nil {
						return err
					}
				}
				v.ids = append(v.ids, id)
				targets = append(targets, target{label: arg, id: id, c: v.c})
			}
			clk := a.deps.clock
			if clk == nil {
				clk = clock.Real()
			}
			since := clk.Now()
			deadline := clk.After(timeout)

			// Subscribe before the first look, so a change between the look and the wait
			// cannot be missed: whatever happens after the subscription is an event, and
			// whatever happened before it is in the state the look reads.
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			wake := make(chan struct{}, 1)
			failed := make(chan error, len(vessels))
			ready := make(chan struct{}, len(vessels))
			for _, v := range vessels {
				go func(v *vessel) {
					c, first := v.subC, true
					for {
						err := c.Stream(ctx, api.MethodEventsSubscribe, api.EventsSubscribeParams{Tickets: v.ids}, func(raw json.RawMessage) error {
							var ev api.Event
							if err := json.Unmarshal(raw, &ev); err != nil {
								return err
							}
							switch ev.Type {
							case api.EventSubscribed:
								if first {
									first = false
									ready <- struct{}{}
								} else {
									select { // anything may have happened while it was down
									case wake <- struct{}{}:
									default:
									}
								}
							case api.EventTimelineEntry:
								select {
								case wake <- struct{}{}:
								default: // one wake-up is already waiting; the look reads everything
								}
							}
							return nil
						})
						if ctx.Err() != nil {
							return
						}
						if first {
							failed <- err // never became live
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-clk.After(waitRetry):
						}
						// The daemon may have restarted: a new connection, and a look at the state.
						var cerr error
						if c, cerr = v.on.connect(ctx, true); cerr != nil {
							failed <- cerr
							return
						}
						defer c.Close()
					}
				}(v)
			}
			for range vessels {
				select {
				case <-ready:
				case err := <-failed:
					return err
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			for {
				var res waitResult
				for _, tg := range targets {
					var t api.TicketGetResult
					if err := tg.c.Call(cmd.Context(), api.MethodTicketGet, api.TicketGetParams{ID: tg.id}, &t); err != nil {
						return err
					}
					res = waitResult{Ticket: tg.label, Summary: t.Summary}
					s := t.Summary
					switch {
					case request && s.PhaseRequest != nil && (!fresh || !s.PhaseRequest.At.Before(since)):
						res.Matched = "phase_request"
					case slices.Contains(phases, string(s.Phase)):
						res.Matched = "phase"
					case s.Review != nil && slices.Contains(reviews, string(s.Review.Status)):
						res.Matched = "review"
					}
					if res.Matched == "" && kind != "" {
						var j api.JournalListResult
						if err := tg.c.Call(cmd.Context(), api.MethodJournalList, api.JournalListParams{Ticket: tg.id, Kind: kind}, &j); err != nil {
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
						break
					}
				}
				s := res.Summary
				if res.Matched != "" {
					if a.json {
						return printJSON(cmd.OutOrStdout(), res)
					}
					out := cmd.OutOrStdout()
					if len(targets) > 1 {
						fmt.Fprintf(out, "%s: ", res.Ticket)
					}
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
				case err := <-failed:
					return err
				case <-wake: // something happened to a watched ticket: look again
				}
			}
		},
	}
	f := cmd.Flags()
	f.StringVar(&kind, "record", "", "a journal record of this kind")
	f.BoolVar(&request, "phase-request", false, "an agent's request to move the ticket")
	f.BoolVar(&fresh, "new", false, "with --phase-request: only a request made after the wait starts")
	f.StringSliceVar(&phases, "phase", nil, "the ticket being in this phase (several: any of them)")
	f.StringSliceVar(&reviews, "review", nil, "the review loop reaching this status (several: any of them)")
	f.DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait")
	return cmd
}
