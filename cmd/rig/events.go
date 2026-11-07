package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newEventsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "events",
		Short: "Print daemon events as they happen (Ctrl-C to stop)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
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
}

func formatEvent(ev api.Event) string {
	subject := ev.Ticket
	if ev.Service != "" {
		subject += "/" + ev.Service
	}
	return strings.TrimSpace(fmt.Sprintf("%s  %s  %s  %s", ev.At.Local().Format("15:04:05"), ev.Type, subject, ev.Data))
}
