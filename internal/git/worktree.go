package git

import (
	"context"
	"fmt"

	"github.com/charlesrobsampson/rig/internal/domain"
)

type worktree struct {
	path   string
	branch string
}

func (w worktree) Kind() domain.CheckoutKind { return domain.CheckoutWorktree }
func (w worktree) Path() string              { return w.path }
func (w worktree) Branch() string            { return w.branch }

// provisionWorktree uses a linked worktree: it shares the repository's object store
// instead of copying it. An existing free branch is reused as it is; a new one
// starts at the base.
func provisionWorktree(ctx context.Context, spec Spec) (Checkout, error) {
	git := Trusted()
	if branchExists(ctx, spec.Repo, spec.Branch) {
		holder, err := branchHolder(ctx, spec.Repo, spec.Branch)
		if err != nil {
			return nil, err
		}
		if holder != "" {
			return nil, fmt.Errorf("branch %s is already checked out at %s", spec.Branch, holder)
		}
		if _, err := git.Run(ctx, spec.Repo, "worktree", "add", spec.Path, spec.Branch); err != nil {
			return nil, err
		}
		return worktree{path: spec.Path, branch: spec.Branch}, nil
	}

	base := spec.Base
	if base == "" {
		var err error
		if base, err = DefaultBranch(ctx, spec.Repo); err != nil {
			return nil, err
		}
	}
	if !resolves(ctx, spec.Repo, base) {
		return nil, fmt.Errorf("base %q was not found in %s", base, spec.Repo)
	}
	if _, err := git.Run(ctx, spec.Repo, "worktree", "add", "-b", spec.Branch, spec.Path, base); err != nil {
		return nil, err
	}
	return worktree{path: spec.Path, branch: spec.Branch}, nil
}
