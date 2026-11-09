package git

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
