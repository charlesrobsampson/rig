package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/charlesrobsampson/rig/internal/domain"
)

// Checkout is a service's working copy of its repository: a worktree on the host,
// or an isolated clone for container agents (docs/formats.md §7).
type Checkout interface {
	Kind() domain.CheckoutKind
	Path() string
	Branch() string

	// IsDirty reports the work that deleting the checkout would lose.
	IsDirty(ctx context.Context) (Dirtiness, error)
	// Import makes the branch's commits available in the main repository: a fetch for a
	// clone (refusing to rewrite the main repo's branch unless force), nothing for a
	// worktree, whose branch already lives there.
	Import(ctx context.Context, force bool) error
	// Remove deletes the checkout, keeping the branch. Without force it refuses with a
	// *DirtyError while any work in it would be lost. Removing a missing checkout is a no-op.
	Remove(ctx context.Context, force bool) error
}

type Spec struct {
	Repo   string // absolute path of the main repository
	Branch string // branch the checkout works on
	Base   string // where a new branch starts; empty means the repository's default branch
	Path   string // absolute directory for the checkout
	Kind   domain.CheckoutKind
}

// Provision creates the checkout. It leaves nothing behind when it fails.
func Provision(ctx context.Context, spec Spec) (Checkout, error) {
	if err := spec.validate(ctx); err != nil {
		return nil, err
	}
	switch spec.Kind {
	case domain.CheckoutWorktree:
		return provisionWorktree(ctx, spec)
	case domain.CheckoutClone:
		return provisionClone(ctx, spec)
	}
	return nil, fmt.Errorf("checkout kind %q is not supported yet", spec.Kind)
}

func (s Spec) validate(ctx context.Context) error {
	switch {
	case !s.Kind.Valid():
		return fmt.Errorf("checkout kind %q is not valid", s.Kind)
	case !filepath.IsAbs(s.Repo):
		return fmt.Errorf("repository path %q must be absolute", s.Repo)
	case !filepath.IsAbs(s.Path):
		return fmt.Errorf("checkout path %q must be absolute", s.Path)
	case s.Branch == "":
		return fmt.Errorf("a branch is required")
	}
	if _, err := Trusted().Run(ctx, s.Repo, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%s is not a git repository: %w", s.Repo, err)
	}
	if _, err := Trusted().Run(ctx, s.Repo, "check-ref-format", "--branch", s.Branch); err != nil {
		return fmt.Errorf("invalid branch name %q", s.Branch)
	}
	return checkPathFree(s.Path)
}

// checkPathFree accepts a missing path or an empty directory.
func checkPathFree(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s already exists and is not a directory", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s already exists and is not empty", path)
	}
	return nil
}
