package lifecycle

import (
	"context"
	"fmt"
	"os"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
)

// ReviewDiff builds, per service, what its checkout has changed since the branch left
// its base: commits, staged and unstaged edits and new files. It reads the checkouts
// through the hardened runner and changes nothing.
func (m *Manager) ReviewDiff(ctx context.Context, p api.ReviewDiffParams) (api.ReviewDiffResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.ReviewDiffResult{}, err
	}
	defer unlock()

	var out api.ReviewDiffResult
	for _, svc := range l.ticket.Services {
		if p.Service != "" && svc.Name != p.Service {
			continue
		}
		out.Services = append(out.Services, m.serviceDiff(ctx, l, svc))
	}
	if p.Service != "" && len(out.Services) == 0 {
		return api.ReviewDiffResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no service %q", p.Ticket, p.Service)}
	}
	return out, nil
}

func (m *Manager) serviceDiff(ctx context.Context, l loaded, svc domain.Service) api.ServiceDiff {
	rec := l.state.Services[svc.Name]
	d := api.ServiceDiff{Service: svc.Name, Branch: svc.Branch, Files: []api.FileChange{}}
	base := firstNonEmptyString(svc.Base, rec.Base)
	if base == "" {
		var err error
		if base, err = git.DefaultBranch(ctx, svc.Repo); err != nil {
			d.Error = err.Error()
			return d
		}
	}
	d.Base = base
	if rec.Checkout.Path == "" {
		d.Note = "no checkout (the ticket is not started, or it was stopped)"
		return d
	}
	if _, err := os.Stat(rec.Checkout.Path); err != nil {
		d.Note = "the checkout directory is missing: " + rec.Checkout.Path
		return d
	}
	// A clone only knows the base as it was when the clone was made, as origin/<base>;
	// a worktree shares the main repository's branches.
	baseRef := base
	if rec.Checkout.Kind == domain.CheckoutClone {
		baseRef = "origin/" + base
	}
	res, err := git.Diff(ctx, rec.Checkout.Path, baseRef)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.MergeBase, d.Patch = res.MergeBase, res.Patch
	for _, f := range res.Files {
		d.Files = append(d.Files, api.FileChange{Path: f.Path, Added: f.Added, Deleted: f.Deleted, Binary: f.Binary, Untracked: f.Untracked})
	}
	return d
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
