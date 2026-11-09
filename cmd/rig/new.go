package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newNewCmd(a *app) *cobra.Command {
	var p api.TicketNewParams
	var services []string
	cmd := &cobra.Command{
		Use:   "new [title]",
		Short: "Create a ticket",
		Long: `Create a ticket: its directory under the rig home with ticket.yaml, plan.md,
tasks.md, manual-tests.md, and state.json (phase "plan").

Each --service is name=repo or name=repo@branch. Without a branch, the service works on
<branch prefix><ticket id>/<service>. Use --from type:ref to seed the ticket from a
tracker (for example jira:PROJ-412); --id, --title and --description override it.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && p.Title == "" {
				p.Title = args[0]
			}
			for _, s := range services {
				spec, err := parseServiceFlag(s)
				if err != nil {
					return usageError(err)
				}
				p.Services = append(p.Services, spec)
			}
			if len(p.Services) == 0 {
				return usageError(fmt.Errorf("at least one --service name=repo is required"))
			}

			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TicketNewResult
			if err := c.Call(cmd.Context(), api.MethodTicketNew, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created ticket %s\n  %s\nnext: refine plan.md, then run `rig start %s`\n", res.Summary.ID, res.Path, res.Summary.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&p.ID, "id", "", "ticket id (default: derived from the title)")
	f.StringVar(&p.Title, "title", "", "ticket title")
	f.StringVar(&p.Description, "description", "", "text for the top of plan.md")
	f.StringVar(&p.From, "from", "", "seed from a tracker, as type:ref (for example jira:PROJ-412)")
	f.StringArrayVar(&services, "service", nil, "a service as name=repo or name=repo@branch (repeatable)")
	return cmd
}

// parseServiceFlag reads name=repo[@branch] and makes the repo path absolute: the
// daemon runs elsewhere, so a relative path would mean something different to it.
func parseServiceFlag(v string) (api.ServiceSpec, error) {
	bad := fmt.Errorf("--service %q must look like name=repo or name=repo@branch", v)
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return api.ServiceSpec{}, bad
	}
	repo, branch := rest, ""
	// A path that exists as written is never split, even if it contains an @.
	if _, err := os.Stat(rest); err != nil {
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			repo, branch = rest[:i], rest[i+1:]
			if repo == "" {
				return api.ServiceSpec{}, bad
			}
			if branch == "" {
				return api.ServiceSpec{}, fmt.Errorf("--service %q has an empty branch after @", v)
			}
		}
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return api.ServiceSpec{}, err
	}
	return api.ServiceSpec{Name: name, Repo: abs, Branch: branch}, nil
}
