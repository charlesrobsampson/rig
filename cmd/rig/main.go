package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/client"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/secrets"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/ticket"
	"github.com/charlesrobsampson/rig/internal/tui"
)

var version = "dev"

// deps are the seams tests replace: how the daemon gets started, and the clock.
type deps struct {
	spawner client.Spawner // nil means start a detached `rig daemon run`
	clock   clock.Clock    // nil means the real clock
	stdin   io.Reader      // nil means os.Stdin; read for confirmation prompts
	// backend runs agents; nil means the one defaults.terminal_backend names.
	backend terminal.Backend
	// docker runs networks and containers; nil means the docker command.
	docker docker.Docker
	// keychain replaces the macOS `security` tool for secret lookups (tests).
	keychain secrets.SecurityRunner
	// delta shows a patch through the `delta` pager; found is false when it is not
	// installed (nil means the real tool).
	delta func(patch string, out io.Writer) (found bool, err error)
	// gh runs the GitHub CLI (nil means the real one).
	gh git.GH
	// exec replaces this process with a command (rig attach); nil means syscall.Exec.
	exec func(argv []string) error
	// isTerminal says whether a person is at a terminal, for the TUI and pagers (nil means: stdin and stdout are
	// terminals); runTUI shows it (nil means the real one).
	isTerminal func() bool
	// page shows text through a pager (rig attach --read); nil means $PAGER, else less.
	page func(text string) error
	// jiraHTTP replaces the HTTP client used to read Jira issues (tests).
	jiraHTTP ticket.HTTPDoer
	runTUI   func(ctx context.Context, o tui.Options) error
	// pickVessel shows the fleet picker (nil means the real one).
	pickVessel func(ctx context.Context, rows []tui.VesselRow) (string, error)
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, deps{}))
}

// run executes one rig invocation and returns its exit code (docs/formats.md §10).
func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	root := newRootCmd(d)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	if d.stdin != nil {
		root.SetIn(d.stdin)
	}
	if err := root.ExecuteContext(ctx); err != nil {
		printError(stderr, err)
		return exitCode(err)
	}
	return 0
}

// now is the CLI's clock: the injected one in tests, the real one otherwise.
func (a *app) now() time.Time {
	if a.deps.clock == nil {
		return clock.Real().Now()
	}
	return a.deps.clock.Now()
}

type app struct {
	deps       deps
	configPath string
	vesselName string
	json       bool
}

func newRootCmd(d deps) *cobra.Command {
	a := &app{deps: d}
	var ticket, tab string
	root := &cobra.Command{
		Use:           "rig",
		Short:         "Run AI coding agents in parallel, each isolated",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.rootRun(cmd, ticket, tab)
		},
	}
	root.Flags().StringVarP(&ticket, "ticket", "t", "", "open the TUI on this ticket")
	root.Flags().StringVar(&tab, "tab", "", "with --ticket: open this tab (plan, tasks, contract, diff, logs, manual)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "global config file (default $RIG_CONFIG, then ~/.config/rig/config.json)")
	root.PersistentFlags().StringVarP(&a.vesselName, "vessel", "V", "", "the vessel (rig instance) to use, by name (default $RIG_VESSEL; see rig fleet)")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print machine-readable JSON")
	root.PersistentPreRunE = func(*cobra.Command, []string) error { return a.chooseVessel() }
	root.AddCommand(newConfigCmd(a), newDaemonCmd(a), newListCmd(a), newEventsCmd(a), newNewCmd(a), newStartCmd(a), newStopCmd(a), newGatesCmd(a), newApproveCmd(a), newPhaseCmd(a), newAgentEventCmd(a), newJournalCmd(a), newAttachCmd(a), newMsgCmd(a), newLogsCmd(a), newReviewCmd(a), newReviewerCmd(a), newWaitCmd(a), newPushCmd(a), newTestsCmd(a), newContractCmd(a), newMCPCmd(a), newReconcileCmd(a), newFleetCmd(a), newStatsCmd(a))
	return root
}
