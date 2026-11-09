package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newStartCmd(a *app) *cobra.Command {
	var sandbox string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "start <ticket-id>",
		Short: "Provision a ticket's checkouts",
		Long: `Provision one checkout of each service's repository inside the ticket directory:
an isolated clone for services whose sandbox is "container" (the default), a git
worktree for "host". Starting is safe to repeat: checkouts that exist are kept.

With --dry-run nothing is provisioned or launched; rig prints the agent launch (command,
working directory, environment) each service would get. Secret values are never shown.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.TicketStartParams{ID: args[0], DryRun: dryRun}
			if sandbox != "" {
				if err := p.Sandbox.UnmarshalText([]byte(sandbox)); err != nil {
					return usageError(fmt.Errorf("--sandbox: %w", err))
				}
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TicketStartResult
			if err := c.Call(cmd.Context(), api.MethodTicketStart, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if dryRun {
				printLaunches(cmd.OutOrStdout(), res.Launches)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tCHECKOUT\tBRANCH\tSTATUS\tPATH")
			for _, co := range res.Checkouts {
				note := "existing"
				if co.Created {
					note = "created"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", co.Service, co.Kind, co.Branch, note, co.Path)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if n := res.Network; n != nil {
				mode := "bridge"
				if n.Internal {
					mode = "internal, egress " + n.Egress
				}
				fmt.Fprintf(cmd.OutOrStdout(), "\nnetwork %s (%s)\n", n.Name, mode)
			}
			printDependencies(cmd.OutOrStdout(), res.Dependencies)
			failedServices := printContainers(cmd.OutOrStdout(), res.Containers)
			if err := printAgents(cmd.OutOrStdout(), res.Agents); err != nil {
				return err
			}
			if failedServices > 0 {
				return fmt.Errorf("%d service(s) did not start; fix the problem and run start again", failedServices)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "override the sandbox for every service: container or host")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show each agent's launch without provisioning or starting anything")
	return cmd
}

// printLaunches shows long arguments (the hooks JSON, the prompt) by size only; the
// full text is in --json output.
func printLaunches(w io.Writer, launches []api.LaunchInfo) {
	for i, l := range launches {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "service %s (%s)\n  dir: %s\n  command:\n", l.Service, l.Harness, l.Dir)
		for _, arg := range l.Command {
			if len(arg) > 80 || strings.Contains(arg, "\n") {
				fmt.Fprintf(w, "    <%d bytes, %d lines>\n", len(arg), strings.Count(arg, "\n")+1)
				continue
			}
			fmt.Fprintf(w, "    %s\n", arg)
		}
		fmt.Fprintln(w, "  env:")
		names := make([]string, 0, len(l.Env))
		for k := range l.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(w, "    %s=%s\n", k, l.Env[k])
		}
	}
}

// printAgents reports each service's agent and fails if any could not start (the
// checkouts are fine; starting again retries).
func printAgents(w io.Writer, agents []api.AgentInfo) error {
	if len(agents) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	failed := 0
	for _, ag := range agents {
		switch {
		case ag.Error != "":
			failed++
			fmt.Fprintf(w, "agent %s: failed: %s\n", ag.Service, ag.Error)
		case ag.Launched:
			fmt.Fprintf(w, "agent %s: started (%s)\n", ag.Service, ag.PaneID)
		default:
			fmt.Fprintf(w, "agent %s: %s\n", ag.Service, ag.Note)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d agent(s) did not start; fix the problem and run start again", failed)
	}
	return nil
}

// printContainers reports each service container and returns how many failed.
func printContainers(w io.Writer, containers []api.ServiceContainerInfo) int {
	if len(containers) == 0 {
		return 0
	}
	fmt.Fprintln(w)
	failed := 0
	for _, c := range containers {
		switch {
		case c.Error != "":
			failed++
			fmt.Fprintf(w, "service %s: failed: %s\n", c.Service, c.Error)
		case c.Running && c.URL != "":
			fmt.Fprintf(w, "service %s: %s at %s\n", c.Service, runningWord(c), c.URL)
		case c.Running:
			fmt.Fprintf(w, "service %s: %s\n", c.Service, runningWord(c))
		default:
			fmt.Fprintf(w, "service %s: %s\n", c.Service, c.Note)
		}
	}
	return failed
}

func runningWord(c api.ServiceContainerInfo) string {
	if c.Note != "" {
		return c.Note
	}
	return "running"
}

func printDependencies(w io.Writer, deps []api.ServiceContainerInfo) {
	if len(deps) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, d := range deps {
		fmt.Fprintf(w, "dependency %s: %s\n", d.Service, runningWord(d))
	}
}
