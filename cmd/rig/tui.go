package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/tui"
)

// rootRun is bare `rig`: the TUI on a terminal, the same table as `rig list` anywhere
// else (a pipe, a script, --json), so output stays scriptable.
func (a *app) rootRun(cmd *cobra.Command, _ []string) error {
	interactive := !a.json
	if interactive {
		if a.deps.isTerminal != nil {
			interactive = a.deps.isTerminal()
		} else {
			interactive = term.IsTerminal(os.Stdout.Fd()) && term.IsTerminal(os.Stdin.Fd())
		}
	}
	if !interactive {
		return newListCmd(a).RunE(cmd, nil)
	}
	opts := tui.Options{Source: daemonSource{a}, Version: version}
	if a.deps.runTUI != nil {
		return a.deps.runTUI(cmd.Context(), opts)
	}
	return tui.Run(cmd.Context(), opts)
}

// daemonSource reads the fleet from the daemon, starting it when it is not running, so
// bare `rig` works from a cold machine and a daemon that dies comes back.
type daemonSource struct{ a *app }

func (s daemonSource) Tickets(ctx context.Context) ([]api.TicketSummary, error) {
	c, err := s.a.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var res api.TicketListResult
	if err := c.Call(ctx, api.MethodTicketList, nil, &res); err != nil {
		return nil, err
	}
	return res.Tickets, nil
}

// File reads one of the ticket's lifecycle files.
func (s daemonSource) File(ctx context.Context, ticket, name string) (string, error) {
	c, err := s.a.connect(ctx, true)
	if err != nil {
		return "", err
	}
	defer c.Close()
	var res api.TicketFileResult
	if err := c.Call(ctx, api.MethodTicketFile, api.TicketFileParams{Ticket: ticket, Name: name}, &res); err != nil {
		return "", missingOr(err)
	}
	return res.Content, nil
}

// Diff is the same text `rig review` prints: each service's stat and patch, through
// delta when it is installed.
func (s daemonSource) Diff(ctx context.Context, ticket string) (string, error) {
	c, err := s.a.connect(ctx, true)
	if err != nil {
		return "", err
	}
	defer c.Close()
	var res api.ReviewDiffResult
	if err := c.Call(ctx, api.MethodReviewDiff, api.ReviewDiffParams{Ticket: ticket}, &res); err != nil {
		return "", missingOr(err)
	}
	var out bytes.Buffer
	view := &cobra.Command{}
	view.SetOut(&out)
	view.SetErr(io.Discard)
	if err := s.a.showDiffs(view, res, false, false); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Logs is the recent output of the ticket's containers (the last 200 lines each).
func (s daemonSource) Logs(ctx context.Context, ticket string) (string, error) {
	c, err := s.a.connect(ctx, true)
	if err != nil {
		return "", err
	}
	defer c.Close()
	var out strings.Builder
	err = c.Stream(ctx, api.MethodLogsStream, api.LogsParams{Ticket: ticket, Tail: 200}, func(raw json.RawMessage) error {
		var l api.LogLine
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		fmt.Fprintf(&out, "%s | %s\n", l.Service, l.Line)
		return nil
	})
	if err != nil {
		return "", missingOr(err)
	}
	if out.Len() == 0 {
		return "", tui.ErrMissing
	}
	return out.String(), nil
}

// missingOr turns the daemon's not_found into the TUI's "does not exist" and leaves
// every other error as it is.
func missingOr(err error) error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.CodeNotFound {
		return tui.ErrMissing
	}
	return err
}
