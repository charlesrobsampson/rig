package main

import (
	"context"
	"io"
	"os"

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
	// isTerminal says whether to show the TUI (nil means: stdin and stdout are
	// terminals); runTUI shows it (nil means the real one).
	isTerminal func() bool
	// jiraHTTP replaces the HTTP client used to read Jira issues (tests).
	jiraHTTP ticket.HTTPDoer
	runTUI   func(ctx context.Context, o tui.Options) error
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

type app struct {
	deps       deps
	configPath string
	json       bool
}

func newRootCmd(d deps) *cobra.Command {
	a := &app{deps: d}
	root := &cobra.Command{
		Use:           "rig",
		Short:         "Run AI coding agents in parallel, each isolated",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageArgs(cobra.NoArgs),
		RunE:          a.rootRun,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "global config file (default $RIG_CONFIG, then ~/.config/rig/config.json)")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print machine-readable JSON")
	root.AddCommand(newConfigCmd(a), newDaemonCmd(a), newListCmd(a), newEventsCmd(a), newNewCmd(a), newStartCmd(a), newStopCmd(a), newGatesCmd(a), newApproveCmd(a), newPhaseCmd(a), newAgentEventCmd(a), newAttachCmd(a), newMsgCmd(a), newLogsCmd(a), newReviewCmd(a), newPushCmd(a), newTestsCmd(a), newContractCmd(a), newMCPCmd(a), newReconcileCmd(a))
	return root
}
