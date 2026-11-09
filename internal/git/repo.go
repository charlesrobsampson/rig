package git

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
)

// CheckRepo reports why dir cannot be used as a main repository, or nil.
func CheckRepo(ctx context.Context, dir string) error {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return errors.New("no such directory")
	}
	if _, err := Trusted().Run(ctx, dir, "rev-parse", "--git-dir"); err != nil {
		return errors.New("not a git repository")
	}
	return nil
}

// Branches lists the repository's local branches, sorted.
func Branches(ctx context.Context, repo string) ([]string, error) {
	out, err := Trusted().Run(ctx, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	sort.Strings(names)
	return names, nil
}

func CurrentCommit(ctx context.Context, dir string) (string, error) {
	return Trusted().Run(ctx, dir, "rev-parse", "HEAD")
}

// DefaultBranch finds the branch new work should start from: what origin's HEAD
// points to, else main, else master. Anything else needs an explicit base.
func DefaultBranch(ctx context.Context, repo string) (string, error) {
	if out, err := Trusted().Run(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimPrefix(out, "origin/"); name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if branchExists(ctx, repo, name) {
			return name, nil
		}
	}
	return "", errors.New("cannot tell which branch is the default (no origin/HEAD, main, or master); set a base branch for this service")
}

func branchExists(ctx context.Context, repo, name string) bool {
	_, err := Trusted().Run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// resolves reports whether rev names a commit.
func resolves(ctx context.Context, repo, rev string) bool {
	_, err := Trusted().Run(ctx, repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil
}

// branchHolder returns where branch is checked out (the main working tree or a
// linked worktree), or "" if nowhere.
func branchHolder(ctx context.Context, repo, branch string) (string, error) {
	out, err := Trusted().Run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	var path string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			return path, nil
		}
	}
	return "", nil
}
