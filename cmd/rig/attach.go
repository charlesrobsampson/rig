package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newAttachCmd(a *app) *cobra.Command {
	var print, read, noPager bool
	var lines int
	cmd := &cobra.Command{
		Use:   "attach <ticket-id> [service]",
		Short: "Jump to a service's agent in your terminal backend",
		Long: `Open the terminal pane where a service's agent runs. The service can be left out
when the ticket has only one. With --print (or --json) the command is shown instead of run.

With --read, rig prints what the agent's screen shows and exits, typing nothing into it:
a look that cannot approve a prompt or interrupt the agent. --lines N prints the last N
lines instead of what is visible. In a terminal it opens in a pager ($PAGER, else less)
at the end of the agent's last 2000 lines; --no-pager prints them instead.`,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines != 0 && !read {
				return usageError(errors.New("--lines needs --read"))
			}
			if lines < 0 {
				return usageError(errors.New("--lines must be positive"))
			}
			paged := read && !a.json && !noPager && a.interactive()
			p := api.AgentAttachParams{Ticket: args[0], Read: read, Lines: lines}
			if paged && lines == 0 {
				p.Lines = pagedLines
			}
			if len(args) == 2 {
				p.Service = args[1]
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.AgentAttachResult
			if err := c.Call(cmd.Context(), api.MethodAgentAttach, p, &res); err != nil {
				return err
			}
			if read && res.Screen == "" {
				// A daemon from before --read ignores it and sends no screen.
				fmt.Fprintln(cmd.ErrOrStderr(), "rig: the agent's screen came back empty; if the daemon predates --read, restart it with: rig daemon stop")
			}
			switch {
			case a.json:
				return printJSON(cmd.OutOrStdout(), res)
			case paged:
				page := a.deps.page
				if page == nil {
					page = runPager
				}
				return page(res.Screen)
			case read:
				fmt.Fprint(cmd.OutOrStdout(), strings.TrimRight(res.Screen, "\n")+"\n")
				return nil
			case print:
				fmt.Fprintln(cmd.OutOrStdout(), strings.Join(res.Command, " "))
				return nil
			}
			run := a.deps.exec
			if run == nil {
				run = execReplace
			}
			return run(res.Command)
		},
	}
	cmd.Flags().BoolVar(&print, "print", false, "print the command instead of running it")
	cmd.Flags().BoolVar(&read, "read", false, "print what the agent's screen shows, without attaching")
	cmd.Flags().BoolVar(&noPager, "no-pager", false, "with --read, print instead of opening a pager")
	cmd.Flags().IntVar(&lines, "lines", 0, "with --read, the last N lines instead of what is visible")
	return cmd
}

// execReplace hands the terminal to the command, so attach behaves as if it had been
// typed directly.
func execReplace(argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("cannot run %q: %w", argv[0], err)
	}
	return syscall.Exec(path, argv, os.Environ())
}

// pagedLines is how much of an agent's scrollback a pager gets: enough to see what it
// did recently, without loading its whole history.
const pagedLines = 2000

// runPager shows text through $PAGER, or less opened at the end; without either the
// text is printed.
func runPager(text string) error {
	argv := []string{"less", "-R", "-F", "+G"}
	if p := os.Getenv("PAGER"); p != "" {
		argv = []string{"sh", "-c", p}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		_, err = fmt.Fprint(os.Stdout, text)
		return err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(text), os.Stdout, os.Stderr
	return cmd.Run()
}
