package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/domain"
)

// Dirtiness is the work a checkout holds that would be lost by deleting it.
type Dirtiness struct {
	Modified   int // tracked files with uncommitted changes, staged or not
	Untracked  int
	Unimported int // commits on the branch the main repository does not have yet
}

// Any reports whether deleting the checkout would lose something.
func (d Dirtiness) Any() bool { return d.Modified+d.Untracked+d.Unimported > 0 }

// DirtyError is why a checkout was not removed.
type DirtyError struct {
	Path string
	Dirtiness
}

func (e *DirtyError) Error() string {
	var parts []string
	if e.Modified > 0 {
		parts = append(parts, fmt.Sprintf("%d modified file(s)", e.Modified))
	}
	if e.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked file(s)", e.Untracked))
	}
	if e.Unimported > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s) not imported into the main repository", e.Unimported))
	}
	msg := fmt.Sprintf("checkout %s still holds work: %s", e.Path, strings.Join(parts, ", "))
	if e.Unimported > 0 {
		msg += "; import the branch first"
	}
	return msg + " (use force to discard it)"
}

// uncommitted counts changed and untracked files in the working tree at path.
func uncommitted(ctx context.Context, r Runner, path string) (modified, untracked int, err error) {
	out, err := r.Run(ctx, path, "status", "--porcelain")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "??"):
			untracked++
		default:
			modified++
		}
	}
	return modified, untracked, nil
}

// Open reattaches to a checkout provisioned earlier, for example after the daemon
// restarted, and refuses a directory that is not a checkout of the stated kind.
func Open(ctx context.Context, spec Spec) (Checkout, error) {
	switch {
	case !spec.Kind.Valid():
		return nil, fmt.Errorf("checkout kind %q is not valid", spec.Kind)
	case !filepath.IsAbs(spec.Repo) || !filepath.IsAbs(spec.Path):
		return nil, fmt.Errorf("repository and checkout paths must be absolute")
	case spec.Branch == "":
		return nil, fmt.Errorf("a branch is required")
	}
	info, err := os.Stat(spec.Path)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("checkout %s does not exist", spec.Path)
	}
	dotGit, err := os.Stat(filepath.Join(spec.Path, ".git"))
	if err != nil {
		return nil, fmt.Errorf("%s is not a %s checkout (no .git)", spec.Path, spec.Kind)
	}
	isWorktree := !dotGit.IsDir() // a linked worktree's .git is a pointer file
	switch {
	case spec.Kind == domain.CheckoutWorktree && isWorktree:
		return worktree{repo: spec.Repo, path: spec.Path, branch: spec.Branch}, nil
	case spec.Kind == domain.CheckoutClone && !isWorktree:
		return clone{repo: spec.Repo, path: spec.Path, branch: spec.Branch}, nil
	}
	return nil, fmt.Errorf("%s is not a %s checkout", spec.Path, spec.Kind)
}
