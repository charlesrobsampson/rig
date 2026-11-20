package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Place puts a file the agent needs but git must never carry (an .npmrc, a local env
// file) into a clone, and makes git ignore it. The clone is agent-writable, so nothing
// in it is followed: a link where the file goes is replaced, and a linked directory on
// the way, or a linked excludes file, is refused.
func (c clone) Place(ctx context.Context, rel string, data []byte) error {
	clean, err := placePath(rel)
	if err != nil {
		return err
	}
	if err := writeInside(c.path, clean, data); err != nil {
		return err
	}
	if err := realDirs(c.path, ".git", "info"); err != nil {
		return err
	}
	return ensureIgnored(ctx, Hardened(), c.path, clean, filepath.Join(c.path, ".git", "info", "exclude"))
}

// Place for a worktree records the exclusion in the main repository, the only place git
// reads a worktree's local excludes from.
func (w worktree) Place(ctx context.Context, rel string, data []byte) error {
	clean, err := placePath(rel)
	if err != nil {
		return err
	}
	if err := writeInside(w.path, clean, data); err != nil {
		return err
	}
	common, err := Trusted().Run(ctx, w.repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(common, "info"), 0o755); err != nil {
		return err
	}
	return ensureIgnored(ctx, Trusted(), w.path, clean, filepath.Join(common, "info", "exclude"))
}

func placePath(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if rel == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return "", fmt.Errorf("%q must be a relative path inside the checkout, outside .git", rel)
	}
	return clean, nil
}

// realDirs checks that each step below root is a directory and not a link, making the
// missing ones.
func realDirs(root string, steps ...string) error {
	dir := root
	for _, step := range steps {
		dir = filepath.Join(dir, step)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case info.Mode()&fs.ModeSymlink != 0 || !info.IsDir():
			return fmt.Errorf("%s is a link or not a directory; rig will not write through it", dir)
		}
	}
	return nil
}

func writeInside(root, clean string, data []byte) error {
	if parent := filepath.Dir(clean); parent != "." {
		if err := realDirs(root, strings.Split(parent, string(filepath.Separator))...); err != nil {
			return err
		}
	}
	target := filepath.Join(root, clean)
	if info, err := os.Lstat(target); err == nil {
		if info.IsDir() {
			return fmt.Errorf("%s is a directory", target)
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	// O_EXCL: if something appeared at the path since, nothing is followed or overwritten.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ensureIgnored adds an anchored pattern for clean to the excludes file, unless git
// already ignores the path.
func ensureIgnored(ctx context.Context, r Runner, checkout, clean, excludeFile string) error {
	if _, err := r.Run(ctx, checkout, "check-ignore", "-q", "--", clean); err == nil {
		return nil
	}
	return appendExclude(excludeFile, "/"+filepath.ToSlash(clean))
}

func appendExclude(file, pattern string) error {
	var existing []byte
	info, err := os.Lstat(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is a link or not a regular file; rig will not write through it", file)
	default:
		if existing, err = os.ReadFile(file); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		pattern = "\n" + pattern
	}
	if _, err := f.WriteString(pattern + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
