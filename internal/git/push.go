package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charlesrobsampson/rig/internal/domain"
)

var (
	// ErrNoRemote: the repository has no such remote to push to.
	ErrNoRemote = errors.New("no git remote")
	// ErrNotImported: the branch is not in the main repository yet; import the checkout first.
	ErrNotImported = errors.New("branch is not in the main repository")
	// ErrDiverged: the base branch has commits on the remote that the branch lacks.
	ErrDiverged = errors.New("base branch has diverged on the remote")
	// ErrRejected: the remote refused the push (usually a non-fast-forward).
	ErrRejected = errors.New("push rejected")
	// ErrMergeConflict: merging the branch into the base would conflict.
	ErrMergeConflict = errors.New("merge conflict")
)

// PushOptions describe one push. Repo is the user's main repository: pushes always run
// there, with the user's own git configuration and credentials, after the checkout's
// branch has been imported into it; never from a clone, which an agent can write and
// which has no access to the remote.
type PushOptions struct {
	Repo   string
	Remote string // default "origin"
	Branch string // the ticket's feature branch
	Base   string // the branch to merge into (merge mode)
	// MergeCommit allows merge mode to create a merge commit when the base has moved on
	// at the remote. Without it, only a fast-forward is pushed.
	MergeCommit bool
}

type PushResult struct {
	Mode   domain.PushMode `json:"mode"`
	Remote string          `json:"remote,omitempty"`
	URL    string          `json:"url,omitempty"`
	Ref    string          `json:"ref,omitempty"` // the remote ref that was updated
	Sha    string          `json:"sha,omitempty"` // what it now points to
	Pushed bool            `json:"pushed"`
	// PRURL is the pull request, in pr mode.
	PRURL string `json:"pr_url,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Push publishes the branch as the push mode says: none reports and pushes nothing,
// push-branch pushes the feature branch, merge puts it into the base branch on the
// remote.
func Push(ctx context.Context, mode domain.PushMode, o PushOptions) (PushResult, error) {
	res := PushResult{Mode: mode}
	tip, err := branchTip(ctx, o)
	if err != nil {
		return res, err
	}
	switch mode {
	case domain.PushNone:
		res.Sha = tip
		res.Note = fmt.Sprintf("push mode none: nothing was pushed; branch %s is in %s for you to handle", o.Branch, o.Repo)
		return res, nil
	case domain.PushBranch:
		return pushBranch(ctx, o, tip, res)
	case domain.PushMerge:
		return pushMerge(ctx, o, tip, res)
	}
	return res, fmt.Errorf("push mode %q is not handled here", mode)
}

func (o *PushOptions) remote() string {
	if o.Remote == "" {
		return "origin"
	}
	return o.Remote
}

func branchTip(ctx context.Context, o PushOptions) (string, error) {
	tip, err := Trusted().Run(ctx, o.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+o.Branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %s is not a branch of %s; import the checkout first", ErrNotImported, o.Branch, o.Repo)
	}
	return tip, nil
}

// remoteURL checks the remote exists and returns where it points.
func remoteURL(ctx context.Context, o PushOptions) (string, error) {
	url, err := Trusted().Run(ctx, o.Repo, "remote", "get-url", o.remote())
	if err != nil {
		remotes, _ := Trusted().Run(ctx, o.Repo, "remote")
		if strings.TrimSpace(remotes) == "" {
			return "", fmt.Errorf("%w: %s has no remote; add one with `git remote add %s <url>`", ErrNoRemote, o.Repo, o.remote())
		}
		return "", fmt.Errorf("%w: %s has no remote named %q (it has: %s)", ErrNoRemote, o.Repo, o.remote(), strings.Join(strings.Fields(remotes), ", "))
	}
	return url, nil
}

func pushBranch(ctx context.Context, o PushOptions, tip string, res PushResult) (PushResult, error) {
	url, err := remoteURL(ctx, o)
	if err != nil {
		return res, err
	}
	res.Remote, res.URL, res.Ref, res.Sha = o.remote(), url, "refs/heads/"+o.Branch, tip
	if err := push(ctx, o, tip, res.Ref); err != nil {
		return res, err
	}
	res.Pushed = true
	return res, nil
}

// push sends sha to a remote ref. It never forces: a remote that has moved on is
// ErrRejected, with advice, and the remote is left as it was.
func push(ctx context.Context, o PushOptions, sha, ref string) error {
	_, err := Trusted().Run(ctx, o.Repo, "push", "--porcelain", o.remote(), sha+":"+ref)
	if err == nil {
		return nil
	}
	var gitErr *Error
	if errors.As(err, &gitErr) {
		text := gitErr.Stderr
		if strings.Contains(text, "rejected") || strings.Contains(text, "non-fast-forward") || strings.Contains(text, "fetch first") {
			return fmt.Errorf("%w: %s on %s has commits this branch does not (not a fast-forward); update the branch from the remote and try again", ErrRejected, ref, o.remote())
		}
	}
	return err
}

func pushMerge(ctx context.Context, o PushOptions, tip string, res PushResult) (PushResult, error) {
	if o.Base == "" {
		return res, errors.New("merge mode needs a base branch")
	}
	url, err := remoteURL(ctx, o)
	if err != nil {
		return res, err
	}
	res.Remote, res.URL, res.Ref = o.remote(), url, "refs/heads/"+o.Base

	tracking := "refs/remotes/" + o.remote() + "/" + o.Base
	if _, err := Trusted().Run(ctx, o.Repo, "fetch", "--no-tags", "--quiet", o.remote(), "+refs/heads/"+o.Base+":"+tracking); err != nil {
		return res, fmt.Errorf("the remote %s has no branch %q to merge into (or cannot be reached): %w", o.remote(), o.Base, err)
	}
	remoteBase, err := Trusted().Run(ctx, o.Repo, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return res, err
	}

	switch {
	case isAncestor(ctx, o.Repo, tip, remoteBase):
		res.Sha = remoteBase
		res.Note = fmt.Sprintf("%s is already part of %s on %s; nothing to push", o.Branch, o.Base, o.remote())
		return res, nil
	case isAncestor(ctx, o.Repo, remoteBase, tip):
		res.Sha = tip // a fast-forward: the base becomes the branch's tip
	case !o.MergeCommit:
		return res, fmt.Errorf("%w: %s on %s has commits %s lacks; merge or rebase %s/%s into %s first, or allow a merge commit for this push",
			ErrDiverged, o.Base, o.remote(), o.Branch, o.remote(), o.Base, o.Branch)
	default:
		merged, err := mergeCommit(ctx, o, remoteBase, tip)
		if err != nil {
			return res, err
		}
		res.Sha = merged
	}
	if err := push(ctx, o, res.Sha, res.Ref); err != nil {
		return res, err
	}
	res.Pushed = true
	return res, nil
}

func isAncestor(ctx context.Context, repo, ancestor, descendant string) bool {
	_, err := Trusted().RunAllowing(ctx, repo, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// mergeCommit merges branch into the remote's base without touching any working tree
// (the user's main repository may have the base checked out): merge-tree builds the
// merged tree, commit-tree wraps it.
func mergeCommit(c context.Context, o PushOptions, remoteBase, tip string) (string, error) {
	tree, err := Trusted().RunAllowing(c, o.Repo, []int{1}, "merge-tree", "--write-tree", "--name-only", remoteBase, tip)
	if err != nil {
		return "", err
	}
	lines := strings.Split(tree, "\n")
	// With conflicts git still exits 1 and prints the tree, a blank line, then the files.
	if len(lines) > 1 {
		var files []string
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "Auto-merging") && !strings.HasPrefix(l, "CONFLICT") {
				files = append(files, l)
			}
		}
		if len(files) > 0 {
			return "", fmt.Errorf("%w: merging %s into %s conflicts in %s; resolve it in the branch and push again", ErrMergeConflict, o.Branch, o.Base, strings.Join(files, ", "))
		}
	}
	sha, err := Trusted().Run(c, o.Repo, "commit-tree", strings.TrimSpace(lines[0]), "-p", remoteBase, "-p", tip, "-m", fmt.Sprintf("Merge branch '%s' into %s", o.Branch, o.Base))
	if err != nil {
		return "", fmt.Errorf("create the merge commit (is git's user.name and user.email set?): %w", err)
	}
	return sha, nil
}
