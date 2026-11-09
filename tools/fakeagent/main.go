// Command fakeagent runs a fakeagent script as a real program, for tests that put an
// "agent" in a terminal pane or a container instead of a real one.
//
//	fakeagent -script script.txt [-state-file path] [-state-cmd "rig agent-event"]
//
// The working directory is the checkout. RIG_SERVICE names the agent's service and
// RIG_TICKET_DIR the ticket directory (default: the parent of the working directory).
// A state command reports through -state-file (the state is written to it), through
// -state-cmd (run with the state appended), and always on the screen as
// "[fakeagent] state=<state>". Exit status: the script's exit code, 2 for a bad
// script or usage, 1 for a failure while running.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/charlesrobsampson/rig/internal/testkit/fakeagent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("fakeagent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	script := fs.String("script", "", "script file to run")
	stateFile := fs.String("state-file", "", "write each reported state to this file")
	stateCmd := fs.String("state-cmd", "", "run this shell command with the state appended")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *script == "" {
		fmt.Fprintln(stderr, "fakeagent: -script is required")
		return 2
	}
	text, err := os.ReadFile(*script)
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}
	cmds, err := fakeagent.Parse(string(text))
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}

	env := fakeagent.Env{
		TicketDir: getenv("RIG_TICKET_DIR"),
		Service:   getenv("RIG_SERVICE"),
		Stdin:     stdin,
		Stdout:    stdout,
		ReportState: func(state string) error {
			if *stateFile != "" {
				if err := os.WriteFile(*stateFile, []byte(state), 0o644); err != nil {
					return err
				}
			}
			if *stateCmd != "" {
				return exec.CommandContext(ctx, "sh", "-c", *stateCmd+" "+state).Run()
			}
			return nil
		},
	}
	err = fakeagent.Run(ctx, cmds, env)
	var exit *fakeagent.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
	}
	fmt.Fprintf(stderr, "fakeagent: %v\n", err)
	return 1
}
