package main

import (
	"context"
	"github.com/charmbracelet/x/term"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
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
	// version replaces the build's version in the daemon handshake (tests); stderr is where
	// the version warning goes.
	version string
	stderr  io.Writer

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
	// termWidth is the terminal's width, 0 when output is not a terminal (nil means:
	// ask the terminal).
	termWidth func() int
	// pickVessel shows the fleet picker (nil means the real one).
	pickVessel func(ctx context.Context, rows []tui.VesselRow) (string, error)
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, deps{}))
}

// run executes one rig invocation and returns its exit code (docs/formats.md §10).
func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	d.stderr = stderr
	root := newRootCmd(d)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	if d.stdin != nil {
		root.SetIn(d.stdin)
	}
	if err := root.ExecuteContext(ctx); err != nil {
		var msg strings.Builder
		printError(&msg, err)
		io.WriteString(stderr, withVessel(msg.String(), root.Annotations["vessel"]))
		return exitCode(err)
	}
	return 0
}

// rigCommand finds a rig command line inside a message (a hint such as "run: rig phase
// t1 push"), so the vessel it was meant for can be put in it.
var rigCommand = regexp.MustCompile("(^|[\\s(`\"'])rig ((?:approve|phase|push|reviewer|review|start|stop|reconcile|contract|attach|msg|journal|logs|tests|gates|wait|watch|list|stats|daemon|new)\\b)")

// withVessel makes the rig commands a message suggests name the vessel they are for:
// followed as printed, a bare "rig start t1" would act on the default vessel.
func withVessel(text, vessel string) string {
	if vessel == "" {
		return text
	}
	return rigCommand.ReplaceAllString(text, "${1}rig -V "+vessel+" ${2}")
}

// now is the CLI's clock: the injected one in tests, the real one otherwise.
func (a *app) now() time.Time {
	if a.deps.clock == nil {
		return clock.Real().Now()
	}
	return a.deps.clock.Now()
}

// termWidth is how wide the terminal is, or 0 when stdout is not one (then nothing is
// shortened, so scripts read whole lines).
func (a *app) termWidth() int {
	if a.deps.termWidth != nil {
		return a.deps.termWidth()
	}
	if !term.IsTerminal(os.Stdout.Fd()) {
		return 0
	}
	w, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		return 0
	}
	return w
}

type app struct {
	deps       deps
	configPath string
	vesselName string
	// vesselFromFlag: the vessel was named with -V (not only $RIG_VESSEL)
	vesselFromFlag bool
	json           bool
	// warned makes the version warning appear once per process, across vessel copies.
	warned *sync.Once
}

func newRootCmd(d deps) *cobra.Command {
	a := &app{deps: d, warned: new(sync.Once)}
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
	root.Annotations = map[string]string{}
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := a.chooseVessel(cmd.Flags().Changed("vessel")); err != nil {
			return err
		}
		if cmd.Annotations["ticket"] == "first" && len(args) > 0 {
			// args is the slice RunE gets: a qualified "vessel/ticket" becomes its id here
			target, id, err := a.ticketRef(args[0])
			if err != nil {
				return err
			}
			*a, args[0] = *target, id
		}
		root.Annotations["vessel"] = a.vesselName
		return nil
	}
	root.AddCommand(newConfigCmd(a), newDaemonCmd(a), newListCmd(a), newEventsCmd(a), newNewCmd(a), newStartCmd(a), newStopCmd(a), newGatesCmd(a), newApproveCmd(a), newPhaseCmd(a), newReworkCmd(a), newAgentEventCmd(a), newJournalCmd(a), newAttachCmd(a), newMsgCmd(a), newLogsCmd(a), newReviewCmd(a), newReviewerCmd(a), newWaitCmd(a), newPushCmd(a), newTestsCmd(a), newContractCmd(a), newMCPCmd(a), newReconcileCmd(a), newFleetCmd(a), newStatsCmd(a), newSyncCmd(a), newTimelineCmd(a))
	// commands whose first argument is a ticket also take "vessel/ticket"
	for _, c := range root.Commands() {
		switch c.Name() {
		case "start", "stop", "phase", "approve", "gates", "push", "review", "reviewer", "journal", "attach", "msg", "logs", "tests", "sync":
			c.Annotations = map[string]string{"ticket": "first"}
		}
	}
	return root
}
