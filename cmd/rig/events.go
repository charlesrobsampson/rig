package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
)

func newEventsCmd(a *app) *cobra.Command {
	var since int
	cmd := &cobra.Command{
		Use:     "watch [ticket...]",
		Aliases: []string{"events"},
		Short:   "Print daemon events as they happen (Ctrl-C to stop)",
		Long: `Print daemon events as they happen. With tickets, only theirs: each timeline entry
(rig timeline) is printed as it is made, a ticket may name its vessel (jybe/j05), and
--since <seq> first prints the entries after that seq, then goes on live with no gap and
no repeat (--since 0 starts from the beginning). Without --since only new entries show.
If the connection drops, watch reconnects and picks up after the last entry it printed.`,
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if since < 0 {
				return usageError(fmt.Errorf("--since must be 0 (everything) or a seq"))
			}
			if cmd.Flags().Changed("since") && len(args) == 0 {
				return usageError(fmt.Errorf("--since needs a ticket to replay"))
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if len(args) > 0 {
				return a.watchTickets(ctx, cmd, args, since, cmd.Flags().Changed("since"))
			}
			c, err := a.connect(ctx, true)
			if err != nil {
				return err
			}
			defer c.Close()

			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			err = c.Stream(ctx, api.MethodEventsSubscribe, nil, func(raw json.RawMessage) error {
				if a.json {
					_, err := fmt.Fprintln(out, string(raw))
					return err
				}
				var ev api.Event
				if err := json.Unmarshal(raw, &ev); err != nil {
					return err
				}
				if ev.Type == api.EventSubscribed {
					fmt.Fprintln(errOut, "listening for events (Ctrl-C to stop)")
					return nil
				}
				_, err := fmt.Fprintln(out, formatEvent(ev))
				return err
			})
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().IntVar(&since, "since", 0, "first print each ticket's timeline entries after this seq")
	return cmd
}

// watchTickets streams the named tickets, one connection per vessel.
func (a *app) watchTickets(ctx context.Context, cmd *cobra.Command, args []string, since int, replay bool) error {
	type vessel struct {
		on     *app
		labels map[string]string // ticket id -> as named
		last   map[string]int    // highest seq printed
	}
	vessels := map[string]*vessel{}
	var order []*vessel
	for _, arg := range args {
		on, id, err := a.ticketRef(arg)
		if err != nil {
			return err
		}
		v := vessels[on.configPath]
		if v == nil {
			v = &vessel{on: on, labels: map[string]string{}, last: map[string]int{}}
			vessels[on.configPath] = v
			order = append(order, v)
		}
		v.labels[id] = arg
	}
	clk := a.deps.clock
	if clk == nil {
		clk = clock.Real()
	}
	var mu sync.Mutex // one line at a time
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	errc := make(chan error, len(order))
	for _, v := range order {
		go func(v *vessel) {
			for {
				c, err := v.on.connect(ctx, true)
				if err != nil {
					errc <- err
					return
				}
				p := api.EventsSubscribeParams{Since: map[string]int{}}
				for id := range v.labels {
					p.Tickets = append(p.Tickets, id)
					if n, ok := v.last[id]; ok {
						p.Since[id] = n // reconnecting: exactly after what was printed
					} else if replay {
						p.Since[id] = since
					}
				}
				err = c.Stream(ctx, api.MethodEventsSubscribe, p, func(raw json.RawMessage) error {
					var ev api.Event
					if err := json.Unmarshal(raw, &ev); err != nil {
						return err
					}
					mu.Lock()
					defer mu.Unlock()
					if ev.Type == api.EventSubscribed && !a.json {
						fmt.Fprintln(errOut, "listening for events (Ctrl-C to stop)")
						return nil
					}
					if ev.Type == api.EventTimelineEntry {
						var d api.TimelineEntryData
						if err := ev.DecodeData(&d); err != nil {
							return err
						}
						v.last[ev.Ticket] = d.Seq
					}
					if a.json {
						_, err := fmt.Fprintln(out, string(raw))
						return err
					}
					if label, ok := v.labels[ev.Ticket]; ok {
						ev.Ticket = label
					}
					_, err := fmt.Fprintln(out, formatEvent(ev))
					return err
				})
				c.Close()
				var ae *api.Error
				if ctx.Err() != nil || (errors.As(err, &ae) && ae.Code != api.CodeUnavailable) {
					errc <- err
					return
				}
				select { // the connection dropped: reconnect where it left off
				case <-ctx.Done():
					errc <- ctx.Err()
					return
				case <-clk.After(time.Second):
				}
			}
		}(v)
	}
	err := <-errc
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func formatEvent(ev api.Event) string {
	subject := ev.Ticket
	if ev.Service != "" {
		subject += "/" + ev.Service
	}
	if ev.Type == api.EventTimelineEntry {
		var d api.TimelineEntryData
		if ev.DecodeData(&d) == nil {
			return strings.TrimSpace(fmt.Sprintf("%s  %s  #%d %s  %s  %s", ev.At.Local().Format("15:04:05"), subject, d.Seq, d.Entry.Kind, d.Entry.By, timelineSummary(d.Entry)))
		}
	}
	return strings.TrimSpace(fmt.Sprintf("%s  %s  %s  %s", ev.At.Local().Format("15:04:05"), ev.Type, subject, ev.Data))
}
