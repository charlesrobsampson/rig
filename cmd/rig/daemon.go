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
	"github.com/charlesrobsampson/rig/internal/contractnotify"
	"github.com/charlesrobsampson/rig/internal/contractwatch"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/docker/dockercli"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/egress"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/gates"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/inject"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/reconcile"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/secrets"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/supervisor"
	"github.com/charlesrobsampson/rig/internal/terminal/backends"
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
	reg := registry.New(g.TicketsDir(), states).WithPendingGate(func(t domain.Ticket, st state.State) domain.Gate {
		gate, _ := gates.Awaiting(g, t, st)
		return gate
	})
	var reconciler *reconcile.Reconciler // set below, once the lifecycle manager exists
	reg.WithDrift(func(id string) []string {
		if reconciler == nil {
			return nil
		}
		return reconciler.Drift(id)
	})
	summaries := newSummaryService(g, secretValues(g, a.deps.keychain), bus, clk, logger)
	reg.WithAISummary(summaries.Info)
	daemon.RegisterTicketHandlers(srv, reg)
	daemon.RegisterTicketCreateHandler(srv, registry.NewCreator(registry.CreatorOptions{
		Dir:          g.TicketsDir(),
		States:       states,
		Sources:      ticketSources(g, secretValues(g, a.deps.keychain), a.deps.jiraHTTP),
		RepoCheck:    func(path string) error { return git.CheckRepo(context.Background(), path) },
		BranchPrefix: g.Defaults.BranchPrefix,
	}), bus)

	backend := a.deps.backend
	if backend == nil {
		if backend, err = backends.New(g.Defaults.TerminalBackend); err != nil {
			return err
		}
	}
	dk := a.deps.docker
	if dk == nil {
		dk = dockercli.New(dockercli.Options{})
	}
	rigBinary, _ := os.Executable()
	agentEnv := map[string]string{"RIG_HOME": g.Paths.Home}
	if a.configPath != "" {
		agentEnv["RIG_CONFIG"] = a.configPath
	}
	sup := supervisor.New(supervisor.Options{Dir: g.TicketsDir(), States: states, Backend: backend, Bus: bus, Clock: clk, Logger: logger, Interval: time.Second})
	injector := inject.New(inject.Options{Dir: g.TicketsDir(), States: states, Backend: backend, Readiness: sup, Clock: clk})
	notifier := &contractnotify.Notifier{Dir: g.TicketsDir(), Sender: injector, Logger: logger}
	lc := lifecycle.New(lifecycle.Options{
		Dir: g.TicketsDir(), States: states, Global: g, Bus: bus, Clock: clk,
		Backend: backend, BackendName: g.Defaults.TerminalBackend, RigBinary: rigBinary, AgentEnv: agentEnv, Docker: dk, Secrets: secretValues(g, a.deps.keychain), GH: a.deps.gh,
		OnContractResolved: notifier.Resolved,
	})
	daemon.RegisterLifecycleHandlers(srv, lc)
	summaries.SetInputs(summaryInputs{lc: lc, reg: reg})
	reconciler = reconcile.New(reconcile.Options{Dir: g.TicketsDir(), States: states, Docker: dk, Backend: backend, Bus: bus, Clock: clk, Logger: logger, Busy: lc.Busy})
	daemon.RegisterReconcileHandler(srv, reconciler)
	daemon.RegisterAgentHandlers(srv, sup)
	daemon.RegisterMessageHandler(srv, injector)

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	supCtx, stopSup := context.WithCancel(ctx)
	supDone, watchDone, contractDone, summaryDone, reconcileDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { sup.Run(supCtx); close(supDone) }()
	cw := contractwatch.New(contractwatch.Options{Dir: g.TicketsDir(), States: states, Bus: bus, Clock: clk, Logger: logger, OnProposal: notifier.NewProposal})
	go func() { cw.Run(supCtx); close(contractDone) }()
	go func() { summaries.Run(supCtx); close(summaryDone) }()
	go func() { reconciler.Run(supCtx); close(reconcileDone) }()
	watcher := &egress.Watcher{Docker: dk, Bus: bus, Clock: clk, Logger: logger}
	go func() { watcher.Run(supCtx); close(watchDone) }()
	defer func() {
		stopSup()
		<-supDone
		<-watchDone
		<-contractDone
		<-summaryDone
		<-reconcileDone
	}()
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

// secretValues is how the daemon finds the values of the secrets tickets name: the
// Keychain first, then the configured .env file.
func secretValues(g config.Global, run secrets.SecurityRunner) func(names []string) (map[string]string, error) {
	keychain := secrets.NewKeychain(g.Secrets.KeychainService)
	if run != nil {
		keychain.Run, keychain.Disabled = run, false
	}
	resolver := secrets.New(keychain, &secrets.EnvFile{Path: g.Secrets.EnvFile})
	return func(names []string) (map[string]string, error) { return resolver.Resolve(context.Background(), names) }
}
