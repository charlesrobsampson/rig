package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func newReviewCmd(a *app) *cobra.Command {
	var stat, plain, move bool
	cmd := &cobra.Command{
		Use:   "review <ticket-id> [service]",
		Short: "Show what a ticket's services changed",
		Long: `Show, per service, everything its checkout has changed since the branch left its
base: commits, staged and unstaged edits and new files. The diff goes through delta
(side by side) when it is installed, otherwise it is printed as a plain git diff.

  --stat   only the list of changed files with their line counts
  --plain  never use delta
  --move   afterwards move the ticket into the review phase (gates still apply)`,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.ReviewDiffParams{Ticket: args[0]}
			if len(args) == 2 {
				p.Service = args[1]
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.ReviewDiffResult
			if err := c.Call(cmd.Context(), api.MethodReviewDiff, p, &res); err != nil {
				return err
			}

			if a.json {
				if err := printJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else if err := a.showDiffs(cmd, res, stat, plain); err != nil {
				return err
			}

			if move {
				var moved api.TransitionResult
				if err := c.Call(cmd.Context(), api.MethodPhaseSet, api.PhaseSetParams{ID: args[0], To: domain.PhaseReview}, &moved); err != nil {
					return err
				}
				if !a.json {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s -> %s\n", args[0], moved.From, moved.To)
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&stat, "stat", false, "show only the changed files and their line counts")
	f.BoolVar(&plain, "plain", false, "print a plain git diff even if delta is installed")
	f.BoolVar(&move, "move", false, "move the ticket into the review phase afterwards")
	return cmd
}

func (a *app) showDiffs(cmd *cobra.Command, res api.ReviewDiffResult, stat, plain bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	warned := false
	for i, d := range res.Services {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "== %s (%s vs %s) ==\n", d.Service, d.Branch, d.Base)
		switch {
		case d.Error != "":
			fmt.Fprintf(out, "cannot show the diff: %s\n", d.Error)
			continue
		case d.Note != "":
			fmt.Fprintln(out, d.Note)
			continue
		case len(d.Files) == 0:
			fmt.Fprintln(out, "no changes")
			continue
		}
		if stat {
			writeStat(out, d)
			continue
		}
		writeStat(out, d)
		fmt.Fprintln(out)
		if plain {
			fmt.Fprintln(out, d.Patch)
			continue
		}
		show := a.deps.delta
		if show == nil {
			show = runDelta
		}
		found, err := show(d.Patch+"\n", out)
		if err != nil {
			return err
		}
		if !found {
			if !warned {
				fmt.Fprintln(errOut, "delta is not installed (https://github.com/dandavison/delta); showing the plain git diff")
				warned = true
			}
			fmt.Fprintln(out, d.Patch)
		}
	}
	return nil
}

func writeStat(out io.Writer, d api.ServiceDiff) {
	width := 0
	for _, f := range d.Files {
		width = max(width, len(f.Path))
	}
	added, deleted := 0, 0
	for _, f := range d.Files {
		note := ""
		switch {
		case f.Binary:
			note = " (binary)"
		case f.Untracked:
			note = " (new, untracked)"
		}
		fmt.Fprintf(out, "  %-*s  +%d -%d%s\n", width, f.Path, f.Added, f.Deleted, note)
		added, deleted = added+f.Added, deleted+f.Deleted
	}
	noun := "files"
	if len(d.Files) == 1 {
		noun = "file"
	}
	fmt.Fprintf(out, "  %d %s changed, +%d -%d\n", len(d.Files), noun, added, deleted)
}

// runDelta pipes a patch through delta. It reports found=false when delta is not
// installed so the caller can fall back.
func runDelta(patch string, out io.Writer) (bool, error) {
	path, err := exec.LookPath("delta")
	if err != nil {
		return false, nil
	}
	cmd := exec.Command(path, "--side-by-side")
	cmd.Stdin = strings.NewReader(patch)
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return true, fmt.Errorf("delta: %w", err)
	}
	return true, nil
}
