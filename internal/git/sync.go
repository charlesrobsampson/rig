package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// BaseStatus is how a ticket's branch stands against its base branch in the main
// repository: how many commits the base has that the branch lacks, and which files
// merging them would conflict in.
type BaseStatus struct {
	Base      string
	Behind    int
	Conflicts []string
}

// baseTip is the base branch's commit in the main repository.
func baseTip(ctx context.Context, repo, base string) (string, error) {
	sha, err := Trusted().Run(ctx, repo, "rev-parse", "--verify", "-q", "refs/heads/"+base+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("base branch %s not found in %s", base, repo)
	}
	return sha, nil
}

// CompareBase measures a checkout against its base. The checkout reads the main
// repository's objects (a clone borrows them, a worktree shares them), so nothing is
// fetched, and merge-tree predicts conflicts without touching the work tree.
func CompareBase(ctx context.Context, repo, checkout, base string) (BaseStatus, error) {
	st := BaseStatus{Base: base}
	baseSha, err := baseTip(ctx, repo, base)
	if err != nil {
		return st, err
	}
	count, err := Hardened().Run(ctx, checkout, "rev-list", "--count", "HEAD.."+baseSha)
	if err != nil {
		return st, err
	}
	if st.Behind, err = strconv.Atoi(count); err != nil || st.Behind == 0 {
		return st, err
	}
	_, err = Hardened().Run(ctx, checkout, "merge-tree", "--write-tree", "--name-only", "--no-messages", "HEAD", baseSha)
	var gitErr *Error
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &gitErr) && errors.As(err, &exit) && exit.ExitCode() == 1:
		// the first line is the merged tree; the rest are the conflicted files
		lines := strings.Split(strings.TrimSpace(gitErr.Stdout), "\n")
		st.Conflicts = append(st.Conflicts, lines[1:]...)
	default:
		return st, err
	}
	return st, nil
}

// MergeBase merges the base branch into the checkout's branch as a commit by identity
// (name, email). A conflict leaves the merge in progress, with markers in the files, for
// the checkout's agent to resolve and commit; the conflicted files are returned.
func MergeBase(ctx context.Context, repo, checkout, base, name, email string) (commit string, conflicts []string, err error) {
	baseSha, err := baseTip(ctx, repo, base)
	if err != nil {
		return "", nil, err
	}
	_, mergeErr := Hardened().Run(ctx, checkout, "-c", "user.name="+name, "-c", "user.email="+email,
		"merge", "--no-edit", "-m", "Merge "+base+" into this branch (rig sync)", baseSha)
	if mergeErr == nil {
		sha, err := Hardened().Run(ctx, checkout, "rev-parse", "HEAD")
		return sha, nil, err
	}
	out, err := Hardened().Run(ctx, checkout, "diff", "--name-only", "--diff-filter=U")
	if err != nil || out == "" {
		return "", nil, fmt.Errorf("merge %s: %w", base, mergeErr)
	}
	return "", strings.Split(out, "\n"), nil
}
