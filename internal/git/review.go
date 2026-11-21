package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ReviewRef is where the main repository keeps the commit a ticket's reviewer is
// looking at: a hidden ref, not a branch, so it never shows up as work to push.
func ReviewRef(ticket string) string { return "refs/rig/review/" + ticket }

// SnapshotForReview gives a reviewer the tip of a coder's branch as a clone of its own
// at dest, and returns that commit. The commit is fetched from the coder's clone into the
// main repository the way Import does, and dest is emptied and cloned again from the main
// repository every round: the reviewer could have changed anything in its copy (its git
// configuration included), so nothing of the last round is reused, and nothing it does
// there can reach the coder. dest itself is kept, because a running reviewer's container
// has it mounted.
func SnapshotForReview(ctx context.Context, repo, coderPath, branch, ticket, dest string) (string, error) {
	if _, err := (clone{repo: repo, path: coderPath, branch: branch}).branchTip(ctx); err != nil {
		return "", err
	}
	ref := ReviewRef(ticket)
	if _, err := Trusted().Run(ctx, repo, "-c", "transfer.fsckObjects=true", "fetch", "-q", "--no-tags", coderPath, "+refs/heads/"+branch+":"+ref); err != nil {
		return "", err
	}
	sha, err := Trusted().Run(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	if err := emptyDir(dest); err != nil {
		return "", err
	}
	source, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", fmt.Errorf("resolve repository %s: %w", repo, err)
	}
	if _, err := Trusted().Run(ctx, repo, "clone", "-q", "--shared", "--no-checkout", "--no-tags", source, dest); err != nil {
		return "", err
	}
	if err := inheritExcludes(ctx, repo, dest); err != nil {
		return "", err
	}
	if _, err := Hardened().Run(ctx, dest, "checkout", "-q", "--detach", sha); err != nil {
		return "", err
	}
	return sha, nil
}

// RemoveReview drops a ticket's review ref and its reviewer's copy.
func RemoveReview(ctx context.Context, repo, ticket, dest string) error {
	ref := ReviewRef(ticket)
	if _, err := Trusted().Run(ctx, repo, "rev-parse", "-q", "--verify", ref); err == nil {
		if _, err := Trusted().Run(ctx, repo, "update-ref", "-d", ref); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(dest); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return os.Remove(dest) // the link only, never what it points to
	}
	return os.RemoveAll(dest)
}

// emptyDir makes dir an empty real directory, removing what is inside (links as links).
func emptyDir(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("the review copy %s is a link; remove it", dir)
	case !info.IsDir():
		return fmt.Errorf("the review copy %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
