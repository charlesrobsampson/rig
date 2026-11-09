package git

import (
	"context"
	"fmt"
	"os"

	"github.com/charlesrobsampson/rig/internal/domain"
)

type worktree struct {
	repo   string
	path   string
	branch string
}

func (w worktree) Kind() domain.CheckoutKind { return domain.CheckoutWorktree }
func (w worktree) Path() string              { return w.path }
func (w worktree) Branch() string            { return w.branch }

// A worktree is on the host, inside the trusted repository, so the trusted runner is used.
func (w worktree) IsDirty(ctx context.Context) (Dirtiness, error) {
	modified, untracked, err := uncommitted(ctx, Trusted(), w.path)
	return Dirtiness{Modified: modified, Untracked: untracked}, err
}

// Import does nothing: commits made in a worktree are already on the main repository's branch.
func (w worktree) Import(context.Context, bool) error { return nil }

func (w worktree) Remove(ctx context.Context, force bool) error {
	if _, err := os.Stat(w.path); os.IsNotExist(err) {
		_, err := Trusted().Run(ctx, w.repo, "worktree", "prune")
		return err
	}
	if !force {
		d, err := w.IsDirty(ctx)
		if err != nil {
			return err
		}
		if d.Any() {
			return &DirtyError{Path: w.path, Dirtiness: d}
		}
	}
	if _, err := Trusted().Run(ctx, w.repo, "worktree", "remove", "--force", w.path); err != nil {
		return err
	}
	_, err := Trusted().Run(ctx, w.repo, "worktree", "prune")
	return err
}

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
		return worktree{repo: spec.Repo, path: spec.Path, branch: spec.Branch}, nil
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
	return worktree{repo: spec.Repo, path: spec.Path, branch: spec.Branch}, nil
}
