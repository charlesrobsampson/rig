package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charlesrobsampson/rig/internal/domain"
)

// ObjectsMounter is implemented by checkouts that read the main repository's objects
// through git alternates, so a container must mount those directories read-only.
type ObjectsMounter interface {
	ObjectsMounts() ([]string, error)
}

type clone struct {
	repo   string
	path   string
	branch string
}

func (c clone) Kind() domain.CheckoutKind { return domain.CheckoutClone }
func (c clone) Path() string              { return c.path }
func (c clone) Branch() string            { return c.branch }

// IsDirty looks inside an agent-writable directory, so every command is hardened.
func (c clone) IsDirty(ctx context.Context) (Dirtiness, error) {
	modified, untracked, err := uncommitted(ctx, Hardened(), c.path)
	if err != nil {
		return Dirtiness{}, err
	}
	unimported, err := c.unimported(ctx)
	if err != nil {
		return Dirtiness{}, err
	}
	return Dirtiness{Modified: modified, Untracked: untracked, Unimported: unimported}, nil
}

// unimported counts the branch's commits the main repository lacks: those not in its
// copy of the branch, or, if it has none, not on any of its branches. The main repo's
// objects are visible to the clone through alternates, so the clone can compare.
func (c clone) unimported(ctx context.Context) (int, error) {
	tip, err := c.branchTip(ctx)
	if err != nil {
		return 0, err
	}
	have, err := Trusted().Run(ctx, c.repo, "for-each-ref", "--format=%(objectname)", "refs/heads/"+c.branch)
	if err != nil {
		return 0, err
	}
	if have == "" {
		if have, err = Trusted().Run(ctx, c.repo, "for-each-ref", "--format=%(objectname)", "refs/heads"); err != nil {
			return 0, err
		}
	}
	args := []string{"rev-list", "--count", tip}
	if have != "" {
		args = append(args, "--not")
		args = append(args, strings.Fields(have)...)
	}
	out, err := Hardened().Run(ctx, c.path, args...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("count unimported commits: %w", err)
	}
	return n, nil
}

func (c clone) branchTip(ctx context.Context) (string, error) {
	tip, err := Hardened().Run(ctx, c.path, "rev-parse", "--verify", "--quiet", "refs/heads/"+c.branch)
	if err != nil {
		return "", fmt.Errorf("branch %s no longer exists in %s", c.branch, c.path)
	}
	return tip, nil
}

// Import fetches the branch into the main repository. The fetch runs in the trusted
// main repo and verifies the objects it receives, since the clone is agent-written.
func (c clone) Import(ctx context.Context, force bool) error {
	if _, err := c.branchTip(ctx); err != nil {
		return err
	}
	if holder, err := branchHolder(ctx, c.repo, c.branch); err != nil {
		return err
	} else if holder != "" {
		return fmt.Errorf("branch %s is checked out at %s in the main repository; switch it to another branch before importing", c.branch, holder)
	}
	ref := "refs/heads/" + c.branch
	refspec := ref + ":" + ref
	if force {
		refspec = "+" + refspec
	}
	_, err := Trusted().Run(ctx, c.repo, "-c", "transfer.fsckObjects=true", "fetch", "--no-tags", c.path, refspec)
	var gitErr *Error
	if errors.As(err, &gitErr) && strings.Contains(gitErr.Stderr, "non-fast-forward") {
		return fmt.Errorf("the main repository's %s has commits the checkout does not (non-fast-forward); import with force to overwrite it", c.branch)
	}
	return err
}

func (c clone) Remove(ctx context.Context, force bool) error {
	if _, err := os.Stat(c.path); os.IsNotExist(err) {
		return nil
	}
	if !force {
		d, err := c.IsDirty(ctx)
		if err != nil {
			return err
		}
		if d.Any() {
			return &DirtyError{Path: c.path, Dirtiness: d}
		}
	}
	return os.RemoveAll(c.path)
}

// ObjectsMounts lists the real host paths of the main repository's object store and
// of any stores it borrows from, in the order to mount them. They are derived from
// the main repository, never from the clone: the agent can edit the clone's
// alternates file, and rig must not mount whatever path it names.
func (c clone) ObjectsMounts() ([]string, error) {
	common, err := Trusted().Run(context.Background(), c.repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	var mounts []string
	seen := map[string]bool{}
	var add func(dir string) error
	add = func(dir string) error {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return fmt.Errorf("resolve object store %s: %w", dir, err)
		}
		if seen[real] {
			return nil
		}
		seen[real] = true
		mounts = append(mounts, real)
		borrowed, err := alternates(real)
		if err != nil {
			return err
		}
		for _, b := range borrowed {
			if err := add(b); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(filepath.Join(common, "objects")); err != nil {
		return nil, err
	}
	return mounts, nil
}

// alternates reads objects/info/alternates; relative entries are relative to objects.
func alternates(objects string) ([]string, error) {
	f, err := os.Open(filepath.Join(objects, "info", "alternates"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var dirs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(objects, line)
		}
		dirs = append(dirs, line)
	}
	return dirs, sc.Err()
}

// provisionClone makes an isolated checkout: its own .git (so the agent can touch
// nothing of the main repository) that shares the main repository's objects through
// alternates instead of copying them. A branch that exists in the main repository is
// continued from its tip; otherwise a new one starts at the base.
func provisionClone(ctx context.Context, spec Spec) (c Checkout, err error) {
	start, err := startPoint(ctx, spec)
	if err != nil {
		return nil, err
	}

	_, statErr := os.Stat(spec.Path)
	preexisting := statErr == nil // checkPathFree already ensured it is empty
	defer func() {
		if err != nil {
			discard(spec.Path, preexisting)
		}
	}()

	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o755); err != nil {
		return nil, err
	}
	if _, err := Trusted().Run(ctx, spec.Repo, "clone", "-q", "--shared", "--no-checkout", "--no-tags", spec.Repo, spec.Path); err != nil {
		return nil, err
	}
	// From here on the directory is agent-writable territory: hardened runner only.
	if _, err := Hardened().Run(ctx, spec.Path, "checkout", "-q", "-b", spec.Branch, start); err != nil {
		return nil, err
	}
	return clone{repo: spec.Repo, path: spec.Path, branch: spec.Branch}, nil
}

// startPoint is the commit the clone's branch begins at.
func startPoint(ctx context.Context, spec Spec) (string, error) {
	rev := spec.Base
	if branchExists(ctx, spec.Repo, spec.Branch) {
		rev = "refs/heads/" + spec.Branch
	} else if rev == "" {
		var err error
		if rev, err = DefaultBranch(ctx, spec.Repo); err != nil {
			return "", err
		}
	}
	sha, err := Trusted().Run(ctx, spec.Repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("base %q was not found in %s", rev, spec.Repo)
	}
	return sha, nil
}

// discard removes what a failed provision created: the directory, or just its contents
// if it was already there.
func discard(path string, keepDir bool) {
	if !keepDir {
		os.RemoveAll(path)
		return
	}
	entries, _ := os.ReadDir(path)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(path, e.Name()))
	}
}
