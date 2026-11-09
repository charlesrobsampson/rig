package main

import (
	"context"
	"log/slog"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/store"
	"github.com/charlesrobsampson/rig/internal/summary"
)

// summaryService builds the background summary job from the config. Anything that keeps
// it from working (an unknown provider, an unreadable store) is logged and turns the job
// off: summaries are advisory and never stop the daemon from starting.
func newSummaryService(g config.Global, secret func(names []string) (map[string]string, error), bus *events.Bus, clk clock.Clock, logger *slog.Logger) *summary.Service {
	var s summary.Summarizer = summary.None{}
	if built, err := summary.New(g.Summarizer, summary.ProviderOptions{APIKey: anthropicKey(secret)}); err != nil {
		logger.Warn("summaries are off", "err", err)
	} else {
		s = built
	}
	var st store.Store
	if s.Name() != "none" {
		fs, err := store.OpenFile(g.StorePath())
		if err != nil {
			logger.Warn("summaries are off: cannot open the store", "err", err)
			s = summary.None{}
		} else {
			st = fs
		}
	}
	return summary.NewService(summary.Options{Dir: g.TicketsDir(), Store: st, Summarizer: s, Bus: bus, Clock: clk, Logger: logger})
}

// anthropicKeyName is the secret the claude summarizer reads, from the Keychain or the
// .env file like any other secret (docs/formats.md §2).
const anthropicKeyName = "ANTHROPIC_API_KEY"

func anthropicKey(secret func(names []string) (map[string]string, error)) func() (string, error) {
	if secret == nil {
		return nil
	}
	return func() (string, error) {
		values, err := secret([]string{anthropicKeyName})
		if err != nil {
			return "", err
		}
		return values[anthropicKeyName], nil
	}
}

// summaryInputs gathers a ticket's plan, tasks and diff through the same code paths the
// CLI uses, so a summary only ever sees what `rig review` would show.
type summaryInputs struct {
	lc  *lifecycle.Manager
	reg *registry.Registry
}

func (s summaryInputs) Gather(ctx context.Context, ticket string) (summary.Input, error) {
	got, err := s.reg.Get(ticket)
	if err != nil {
		return summary.Input{}, err
	}
	in := summary.Input{Ticket: ticket, Title: got.Summary.Title}
	in.Plan, _ = s.reg.File(ticket, "plan.md") // a missing file is just empty
	in.Tasks, _ = s.reg.File(ticket, "tasks.md")
	diffs, err := s.lc.ReviewDiff(ctx, api.ReviewDiffParams{Ticket: ticket})
	if err != nil {
		return in, nil // no checkouts yet: the plan and tasks still make a summary
	}
	for _, d := range diffs.Services {
		if d.Error != "" || d.Note != "" || len(d.Files) == 0 {
			continue
		}
		sd := summary.ServiceDiff{Service: d.Service, Patch: d.Patch}
		for _, f := range d.Files {
			sd.Files = append(sd.Files, f.Path)
			sd.Stat.Files++
			sd.Stat.Added += f.Added
			sd.Stat.Deleted += f.Deleted
		}
		in.Diffs = append(in.Diffs, sd)
	}
	return in, nil
}
