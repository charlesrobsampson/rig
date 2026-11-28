package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/dockercli"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/egress"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/gates"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/inject"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/phasenotify"
	"github.com/charlesrobsampson/rig/internal/reconcile"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/secrets"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/supervisor"
	"github.com/charlesrobsampson/rig/internal/terminal/backends"
	"github.com/charlesrobsampson/rig/internal/timeline"
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
	ver := version
	if a.deps.version != "" {
		ver = a.deps.version
	}
	opts := client.Options{
		SocketPath: g.SocketPath(),
		PIDPath:    g.PIDPath(),
		Version:    ver,
		Clock:      a.deps.clock,
		StartHint:  "see " + g.LogPath(),
	}
	if autostart {
		opts.Spawner = a.deps.spawner
		if opts.Spawner == nil {
			opts.Spawner = spawnDetached(a.configPath)
		}
	}
	c, err := client.Connect(ctx, opts)
	if err == nil && a.deps.stderr != nil && a.warned != nil {
		if msg := client.VersionMismatch(ver, c.DaemonVersion()); msg != "" {
			a.warned.Do(func() { fmt.Fprintln(a.deps.stderr, withVessel(msg, a.vesselName)) })
		}
	}
	return c, err
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
	var force bool
	restart := &cobra.Command{
		Use:   "restart",
		Short: "Stop the daemon and start it again from this rig",
		Long: `Stops the vessel's daemon and starts it from this rig binary, so it runs the build
you have. Refuses, naming the tickets, while a review round is being started or a ticket
is being started, stopped or synced, since a restart would cut that off; --force
restarts anyway. A review round already running is not lost: after the restart it reads
as awaiting_ready, and review.ready or "rig reviewer <ticket> --start" starts it again.
A daemon that is not running is just started.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.restartDaemon(cmd, force) },
	}
	restart.Flags().BoolVar(&force, "force", false, "restart even with work in flight")
	cmd.AddCommand(restart)
	return cmd
}

func (a *app) restartDaemon(cmd *cobra.Command, force bool) error {
	c, err := a.connect(cmd.Context(), false)
	var apiErr *api.Error
	switch {
	case errors.As(err, &apiErr) && apiErr.Code == api.CodeNotRunning:
		return a.startDaemon(cmd, nil)
	case err != nil:
		return err
	}
	if !force {
		var busy api.DaemonBusyResult
		switch err := c.Call(cmd.Context(), api.MethodDaemonBusy, nil, &busy); {
		case errors.As(err, &apiErr) && apiErr.Code == api.CodeUnavailable:
			c.Close()
			return &api.Error{Code: api.CodeConflict, Message: "this daemon is too old to say what it is doing, so a restart cannot be checked; use --force to restart anyway (a ticket being started or stopped, or a review round being started, would be cut off)"}
		case err != nil:
			c.Close()
			return err
		}
		if len(busy.Tickets) > 0 {
			c.Close()
			var ids, details []string
			for _, t := range busy.Tickets {
				ids = append(ids, t.Ticket)
				details = append(details, fmt.Sprintf("%s: %s", t.Ticket, t.Doing))
			}
			return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("the daemon is busy with %s; nothing was restarted. Wait for it, or use --force", strings.Join(ids, ", ")), Details: details}
		}
	}
	err = c.Stop(cmd.Context())
	c.Close()
	if err != nil {
		return err
	}
	return a.startDaemon(cmd, nil)
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
	daemon.RegisterEventHandlersWith(srv, bus, daemon.EventsOptions{TicketsDir: g.TicketsDir()})
	states := state.NewManager(clk)
	reg := registry.New(g.TicketsDir(), states).WithClock(clk.Now).WithPendingGate(func(t domain.Ticket, st state.State) domain.Gate {
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
	reg.WithPids(func(id string) map[string]int {
		if reconciler == nil {
			return nil
		}
		return reconciler.Pids(id)
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
	dk = docker.Scoped(dk, g.Paths.Home)
	rigBinary, _ := os.Executable()
	agentEnv := map[string]string{"RIG_HOME": g.Paths.Home}
	if a.configPath != "" {
		agentEnv["RIG_CONFIG"] = a.configPath
	}
	tl := timeline.New(clk.Now) // the one writer of every ticket's timeline.jsonl
	tl.Logger = logger
	tl.OnAppend = func(dir string, e timeline.Entry) {
		ticket := filepath.Base(dir)
		bus.Publish(api.EventTimelineEntry, ticket, e.Service, api.TimelineEntryData{Ticket: ticket, Seq: e.Seq, Entry: api.TimelineEntry(e)})
	}
	var lc *lifecycle.Manager // set below; the supervisor starts polling only after it is
	var phaseNotifier *phasenotify.Notifier
	sup := supervisor.New(supervisor.Options{Dir: g.TicketsDir(), States: states, Backend: backend, Bus: bus, Clock: clk, Timeline: tl, Logger: logger, Interval: time.Second,
		PhaseRequest: func(ticket, service string, to domain.Phase, reason string) error {
			return lc.AskForPhase(ticket, service, to, reason)
		},
		Question: func(ticket, service, text string) error {
			return lc.AskQuestion(ticket, service, text)
		},
		Review: func(ticket, service, kind, text string, data json.RawMessage) error {
			return lc.ReviewRecord(ticket, service, kind, text, data)
		},
		Tell: func(ticket, service, text string) { phaseNotifier.Deliver(ticket, service, text) }})
	injector := inject.New(inject.Options{Dir: g.TicketsDir(), States: states, Backend: backend, Readiness: sup, Clock: clk, Timeline: tl,
		Answered: func(ticket, service string) {
			if err := lc.AnswerQuestion(ticket, service); err != nil {
				logger.Warn("close an answered question", "ticket", ticket, "service", service, "err", err)
			}
		}})
	notifier := &contractnotify.Notifier{Dir: g.TicketsDir(), Sender: injector, Logger: logger}
	phaseNotifier = phasenotify.New(phasenotify.Options{Sender: injector, Clock: clk, Logger: logger})
	defer phaseNotifier.Close()
	lc = lifecycle.New(lifecycle.Options{
		Dir: g.TicketsDir(), States: states, Global: g, Bus: bus, Clock: clk, Timeline: tl,
		Backend: backend, BackendName: g.Defaults.TerminalBackend, RigBinary: rigBinary, AgentEnv: agentEnv, Docker: dk, Secrets: secretValues(g, a.deps.keychain), GH: a.deps.gh,
		OnContractResolved: notifier.Resolved,
		OnPhaseChanged:     phaseNotifier.PhaseChanged,
		Deliver:            phaseNotifier.Deliver,
	})
	defer lc.Close()
	reg.WithBase(func(t domain.Ticket, st state.State) []api.BaseInfo {
		return lc.BaseStatus(context.Background(), t, st)
	})
	reg.WithAgentSeconds(lc.AgentSeconds)
	reg.WithRoundPending(lc.RoundPending)
	reg.WithReviewStale(func(t domain.Ticket, st state.State) bool {
		return lc.ReviewStale(context.Background(), t, st)
	})
	reg.WithTouched(func(t domain.Ticket, st state.State) []api.TouchedInfo {
		return lc.Touched(context.Background(), t, st)
	})
	daemon.RegisterLifecycleHandlers(srv, lc)
	daemon.RegisterTimelineHandler(srv, g.TicketsDir())
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

// platformStore is the machine's own secret store: the macOS Keychain, or the Secret
// Service through secret-tool on Linux; anywhere else it finds nothing and only the .env
// file is used. A test passes a runner to stand in for the real tool; it then always
// speaks the Keychain's protocol, whatever the platform, so the suite is the same everywhere.
func platformStore(service string, run secrets.SecurityRunner) secrets.Source {
	if run != nil {
		k := secrets.NewKeychain(service)
		k.Run, k.Disabled = run, false
		return k
	}
	if runtime.GOOS == "linux" {
		return secrets.NewSecretTool(service)
	}
	return secrets.NewKeychain(service)
}

// secretValues is how the daemon finds the values of the secrets tickets name: the
// platform's secret store first (the Keychain, or the Secret Service on Linux), then the
// configured .env file.
func secretValues(g config.Global, run secrets.SecurityRunner) func(names []string) (map[string]string, error) {
	resolver := secrets.New(platformStore(g.Secrets.KeychainService, run), &secrets.EnvFile{Path: g.Secrets.EnvFile})
	return func(names []string) (map[string]string, error) { return resolver.Resolve(context.Background(), names) }
}
