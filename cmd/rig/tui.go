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
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/tui"
)

// interactive says whether a person is at a terminal: stdin and stdout both are.
func (a *app) interactive() bool {
	if a.deps.isTerminal != nil {
		return a.deps.isTerminal()
	}
	return term.IsTerminal(os.Stdout.Fd()) && term.IsTerminal(os.Stdin.Fd())
}

// rootRun is bare `rig`: the TUI on a terminal, the same table as `rig list` anywhere
// else (a pipe, a script, --json), so output stays scriptable. With several vessels and
// none chosen, a picker comes first, and F in the TUI comes back to it.
func (a *app) rootRun(cmd *cobra.Command, ticket, tab string) error {
	if a.json || !a.interactive() {
		return newListCmd(a).RunE(cmd, nil)
	}
	var open *tui.Target
	switch {
	case ticket != "":
		t := tui.TabPlan
		if tab != "" {
			var err error
			if t, err = tui.ParseTab(tab); err != nil {
				return usageError(err)
			}
		}
		open = &tui.Target{Ticket: ticket, Tab: t}
	case tab != "":
		return usageError(errors.New("--tab needs --ticket"))
	}
	folds := map[string]tui.Folds{} // kept across the vessel picker, gone when rig exits
	chosen := a.configPath != "" || os.Getenv("RIG_CONFIG") != ""
	for {
		opts := tui.Options{Source: daemonSource{a}, Version: version, Vessel: a.vesselName, Open: open, Folds: folds}
		if chosen {
			fleet, user, err := loadFleet()
			if err != nil {
				return err
			}
			opts.CanSwitch = len(visibleVessels(fleet, user)) > 1
		} else {
			name, many, err := a.pickVessel(cmd)
			if err != nil || name == "" && many {
				return err
			}
			opts.Vessel, opts.CanSwitch = name, many
		}
		run := tui.Run
		if a.deps.runTUI != nil {
			run = a.deps.runTUI
		}
		if err := run(cmd.Context(), opts); !errors.Is(err, tui.ErrSwitchVessel) {
			return err
		}
		chosen, open = false, nil // from now on the picker chooses; the deep link was for the first vessel
	}
}

// pickVessel chooses the vessel when none was: the only one there is, or the person's pick
// from several (many is true then; "" means they quit). The choice becomes the config.
func (a *app) pickVessel(cmd *cobra.Command) (name string, many bool, err error) {
	fleet, user, err := loadFleet()
	if err != nil {
		return "", false, err
	}
	all := visibleVessels(fleet, user)
	switch len(all) {
	case 0:
		return "", false, nil
	case 1:
		a.configPath, a.vesselName = all[0].Config, all[0].Name
		return all[0].Name, false, nil
	}
	rows := make([]tui.VesselRow, len(all))
	for i, h := range all {
		info, _ := a.inspectVessel(cmd, h)
		row := tui.VesselRow{Name: h.Name, Status: "stopped", Tickets: "-"}
		switch {
		case info.Error != "":
			row.Status = "error: " + info.Error
		case info.Running:
			row.Status = "running (" + info.Version + ")"
		}
		if info.Tickets != nil {
			row.Tickets = info.Standing
		}
		rows[i] = row
	}
	pick := tui.PickVessel
	if a.deps.pickVessel != nil {
		pick = a.deps.pickVessel
	}
	if name, err = pick(cmd.Context(), rows); err != nil || name == "" {
		return "", true, err
	}
	for _, h := range all {
		if h.Name == name {
			a.configPath, a.vesselName = h.Config, h.Name
		}
	}
	return name, true, nil
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

// call runs one daemon request, starting the daemon if needed, like every CLI command.
func (s daemonSource) call(ctx context.Context, method string, params, result any) error {
	c, err := s.a.connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Call(ctx, method, params, result)
}

func (s daemonSource) Approve(ctx context.Context, ticket string, gate domain.Gate) error {
	return s.call(ctx, api.MethodGateApprove, api.GateApproveParams{ID: ticket, Gate: gate}, nil)
}

func (s daemonSource) MovePhase(ctx context.Context, ticket string, to domain.Phase) error {
	return s.call(ctx, api.MethodPhaseSet, api.PhaseSetParams{ID: ticket, To: to}, nil)
}

func (s daemonSource) Push(ctx context.Context, ticket string) (api.PushRunResult, error) {
	var res api.PushRunResult
	err := s.call(ctx, api.MethodPushRun, api.PushRunParams{ID: ticket}, &res)
	return res, err
}

func (s daemonSource) Stop(ctx context.Context, ticket string, force bool) (api.TicketStopResult, error) {
	var res api.TicketStopResult
	err := s.call(ctx, api.MethodTicketStop, api.TicketStopParams{ID: ticket, Force: force}, &res)
	return res, err
}

func (s daemonSource) Message(ctx context.Context, ticket, service, text string) error {
	return s.call(ctx, api.MethodAgentMessage, api.AgentMessageParams{Ticket: ticket, Service: service, Text: text}, nil)
}

func (s daemonSource) Proposals(ctx context.Context, ticket string) ([]api.ContractProposal, error) {
	var res api.ContractListResult
	if err := s.call(ctx, api.MethodContractList, api.ContractListParams{Ticket: ticket}, &res); err != nil {
		return nil, missingOr(err)
	}
	return res.Proposals, nil
}

func (s daemonSource) Resolve(ctx context.Context, ticket, proposal string, accept bool, reason string) error {
	method := api.MethodContractReject
	if accept {
		method = api.MethodContractAccept
	}
	return s.call(ctx, method, api.ContractResolveParams{Ticket: ticket, Proposal: proposal, Reason: reason}, nil)
}

func (s daemonSource) AttachCommand(ctx context.Context, ticket, service string) ([]string, error) {
	var res api.AgentAttachResult
	if err := s.call(ctx, api.MethodAgentAttach, api.AgentAttachParams{Ticket: ticket, Service: service}, &res); err != nil {
		return nil, err
	}
	return res.Command, nil
}

// Manual is the ticket's manual tests with live addresses filled in.
func (s daemonSource) Manual(ctx context.Context, ticket string) (api.TestsRenderResult, error) {
	var res api.TestsRenderResult
	if err := s.call(ctx, api.MethodTestsRender, api.TestsRenderParams{Ticket: ticket}, &res); err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == api.CodeInvalid && len(apiErr.Details) > 0 {
			return res, fmt.Errorf("%s: %s", apiErr.Message, strings.Join(apiErr.Details, "; "))
		}
		return res, missingOr(err)
	}
	return res, nil
}

func (s daemonSource) SetStep(ctx context.Context, ticket, step string, done bool) error {
	return s.call(ctx, api.MethodTestsStep, api.TestsStepParams{Ticket: ticket, Step: step, Done: done}, nil)
}

func (s daemonSource) SignOff(ctx context.Context, ticket string, override bool) error {
	return s.call(ctx, api.MethodTestsSignoff, api.TestsSignoffParams{Ticket: ticket, Override: override}, nil)
}

// Subscribe opens the daemon's event stream and returns once the daemon says it is
// live. The channel closes when the stream ends: the daemon went away, or it dropped
// this client for being too slow.
func (s daemonSource) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	c, err := s.a.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	out := make(chan api.Event, 64)
	ready := make(chan error, 1)
	go func() {
		defer c.Close()
		defer close(out)
		live := false
		err := c.Stream(ctx, api.MethodEventsSubscribe, nil, func(raw json.RawMessage) error {
			var ev api.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				return err
			}
			if ev.Type == api.EventSubscribed {
				live = true
				ready <- nil
				return nil
			}
			select {
			case out <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if !live {
			if err == nil {
				err = errors.New("the daemon closed the event stream before it was live")
			}
			ready <- err
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
		return out, nil
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
}
