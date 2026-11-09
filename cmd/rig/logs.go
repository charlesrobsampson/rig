package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newLogsCmd(a *app) *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <ticket-id> [service]",
		Short: "Show container logs, each line prefixed with its service",
		Long: `Show the output of a ticket's containers: its services, its dependencies, and the
egress proxy ("proxy"). Name one to see only it. Containers that have exited still
have their logs. With -f the output follows until Ctrl-C.`,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tail < 0 {
				return usageError(fmt.Errorf("--tail must be 0 or more"))
			}
			p := api.LogsParams{Ticket: args[0], Tail: tail, Follow: follow}
			if len(args) == 2 {
				p.Service = args[1]
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c, err := a.connect(ctx, true)
			if err != nil {
				return err
			}
			defer c.Close()

			out := cmd.OutOrStdout()
			err = c.Stream(ctx, api.MethodLogsStream, p, func(raw json.RawMessage) error {
				if a.json {
					_, err := fmt.Fprintln(out, string(raw))
					return err
				}
				var l api.LogLine
				if err := json.Unmarshal(raw, &l); err != nil {
					return err
				}
				_, err := fmt.Fprintf(out, "%s | %s\n", l.Service, l.Line)
				return err
			})
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new lines until Ctrl-C")
	cmd.Flags().IntVar(&tail, "tail", 0, "show only the last N lines of each container (0 = all)")
	return cmd
}
