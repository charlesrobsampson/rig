package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/client"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

func (a *app) global() (config.Global, error) {
	return config.LoadGlobal(config.GlobalOptions{Path: a.configPath})
}

// connect opens the daemon connection every daemon-backed command uses. With
// autostart, a missing daemon is started first.
func (a *app) connect(ctx context.Context, autostart bool) (*client.Client, error) {
	g, err := a.global()
	if err != nil {
		return nil, err
	}
	opts := client.Options{
		SocketPath: g.SocketPath(),
		Version:    version,
		Clock:      a.deps.clock,
		StartHint:  "see " + g.LogPath(),
	}
	if autostart {
		opts.Spawner = a.deps.spawner
		if opts.Spawner == nil {
			opts.Spawner = spawnDetached(a.configPath)
		}
	}
	return client.Connect(ctx, opts)
}

func newDaemonCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the rig daemon (rigd)",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		&cobra.Command{Use: "run", Short: "Run the daemon in the foreground", Args: usageArgs(cobra.NoArgs), RunE: a.runDaemon},
		&cobra.Command{Use: "start", Short: "Start the daemon in the background if it is not running", Args: usageArgs(cobra.NoArgs), RunE: a.startDaemon},
		&cobra.Command{Use: "status", Short: "Show whether the daemon is running", Args: usageArgs(cobra.NoArgs), RunE: a.daemonStatus},
		&cobra.Command{Use: "stop", Short: "Stop the daemon", Args: usageArgs(cobra.NoArgs), RunE: a.stopDaemon},
	)
	return cmd
}

func (a *app) runDaemon(cmd *cobra.Command, _ []string) error {
	g, err := a.global()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(g.Paths.Home, 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(g.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, nil))
	clk := a.deps.clock
	if clk == nil {
		clk = clock.Real()
	}

	srv, err := daemon.Listen(daemon.Options{
		SocketPath: g.SocketPath(),
		PIDPath:    g.PIDPath(),
		Version:    version,
		Clock:      clk,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("daemon did not start", "err", err)
		return err
	}
	srv.Handle(api.MethodDaemonStop, func(context.Context, json.RawMessage) (any, error) {
		srv.Stop()
		return api.EmptyData{}, nil
	})
	bus := events.NewBus(clk, 0)
	daemon.RegisterEventHandlers(srv, bus)
	states := state.NewManager(clk)
	daemon.RegisterTicketHandlers(srv, registry.New(g.TicketsDir(), states))
	daemon.RegisterTicketCreateHandler(srv, registry.NewCreator(registry.CreatorOptions{
		Dir:          g.TicketsDir(),
		States:       states,
		Sources:      ticket.DefaultRegistry(),
		RepoCheck:    func(path string) error { return git.CheckRepo(context.Background(), path) },
		BranchPrefix: g.Defaults.BranchPrefix,
	}), bus)
	daemon.RegisterLifecycleHandlers(srv, lifecycle.New(lifecycle.Options{Dir: g.TicketsDir(), States: states, Global: g, Bus: bus, Clock: clk}))

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx)
}

func (a *app) startDaemon(cmd *cobra.Command, _ []string) error {
	c, err := a.connect(cmd.Context(), true)
	if err != nil {
		return err
	}
	defer c.Close()
	return a.printStatus(cmd, c)
}

func (a *app) daemonStatus(cmd *cobra.Command, _ []string) error {
	c, err := a.connect(cmd.Context(), false)
	if err != nil {
		return err
	}
	defer c.Close()
	return a.printStatus(cmd, c)
}

func (a *app) printStatus(cmd *cobra.Command, c *client.Client) error {
	status, err := c.Status(cmd.Context())
	if err != nil {
		return err
	}
	if a.json {
		return printJSON(cmd.OutOrStdout(), status)
	}
	up := time.Duration(status.UptimeSeconds) * time.Second
	fmt.Fprintf(cmd.OutOrStdout(), "rigd %s running (pid %d, up %s)\n", status.Version, status.PID, up)
	return nil
}

func (a *app) stopDaemon(cmd *cobra.Command, _ []string) error {
	c, err := a.connect(cmd.Context(), false)
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.CodeNotRunning {
		fmt.Fprintln(cmd.OutOrStdout(), "daemon is not running")
		return nil
	}
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Stop(cmd.Context()); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "daemon stopped")
	return nil
}
