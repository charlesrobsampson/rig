package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileChange is one file's change in a review diff.
type FileChange struct {
	Path      string `json:"path"`
	Added     int    `json:"added"`
	Deleted   int    `json:"deleted"`
	Binary    bool   `json:"binary,omitempty"`
	Untracked bool   `json:"untracked,omitempty"`
}

// DiffResult is everything a service's checkout has changed since the branch left its
// base: commits, staged and unstaged edits, and files git does not track yet.
type DiffResult struct {
	MergeBase string       `json:"merge_base"`
	Files     []FileChange `json:"files"`
	Patch     string       `json:"patch"`
}

// maxUntrackedPatch bounds how much of one new file is shown; a build output or a
// dataset the agent forgot to ignore must not turn a review into a wall of text.
const maxUntrackedPatch = 256 * 1024

// Diff compares the checkout's working tree with the point where its branch left
// baseRef (the merge base), so committed and uncommitted work both show. It runs
// through the hardened runner: the directory is written by an agent, so no hook,
// external diff program or text conversion in it may run on the host. The checkout's
// index and files are never changed.
func Diff(ctx context.Context, dir, baseRef string) (DiffResult, error) {
	g := Hardened()
	base, err := g.Run(ctx, dir, "merge-base", "HEAD", baseRef)
	if err != nil {
		return DiffResult{}, fmt.Errorf("find where the branch left %s: %w", baseRef, err)
	}
	patch, err := g.Run(ctx, dir, "diff", "--no-color", "--find-renames", base)
	if err != nil {
		return DiffResult{}, err
	}
	numstat, err := g.Run(ctx, dir, "diff", "--numstat", "--find-renames", "-z", base)
	if err != nil {
		return DiffResult{}, err
	}
	res := DiffResult{MergeBase: base, Files: parseNumstat(numstat), Patch: patch}

	untracked, err := g.Run(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return DiffResult{}, err
	}
	for _, path := range strings.Split(untracked, "\x00") {
		if path == "" {
			continue
		}
		change := FileChange{Path: path, Untracked: true}
		if info, err := os.Lstat(filepath.Join(dir, path)); err == nil && info.Size() > maxUntrackedPatch {
			res.Files = append(res.Files, change)
			res.Patch = appendPatch(res.Patch, fmt.Sprintf("new file %s is %d bytes; not shown", path, info.Size()))
			continue
		}
		// --no-index diffs a file against nothing; it exits 1 because they differ.
		fileStat, err := g.RunAllowing(ctx, dir, []int{1}, "diff", "--no-index", "--numstat", "--", "/dev/null", path)
		if err != nil {
			return DiffResult{}, err
		}
		if files := parseNumstat(fileStat); len(files) == 1 {
			change.Added, change.Deleted, change.Binary = files[0].Added, files[0].Deleted, files[0].Binary
		}
		filePatch, err := g.RunAllowing(ctx, dir, []int{1}, "diff", "--no-index", "--no-color", "--", "/dev/null", path)
		if err != nil {
			return DiffResult{}, err
		}
		res.Files = append(res.Files, change)
		res.Patch = appendPatch(res.Patch, filePatch)
	}
	return res, nil
}

func appendPatch(patch, more string) string {
	if patch == "" {
		return more
	}
	return patch + "\n" + more
}

// parseNumstat reads `git diff --numstat` output, NUL-separated (-z) or by lines:
// "<added>\t<deleted>\t<path>", with "-" counts for binary files. For a rename -z gives
// an empty path followed by the old and new names; the new name is kept.
func parseNumstat(out string) []FileChange {
	var files []FileChange
	sep := "\n"
	if strings.Contains(out, "\x00") {
		sep = "\x00"
	}
	parts := strings.Split(out, sep)
	for i := 0; i < len(parts); i++ {
		fields := strings.SplitN(parts[i], "\t", 3)
		if len(fields) != 3 {
			continue
		}
		path := fields[2]
		if path == "" && sep == "\x00" && i+2 < len(parts) { // a rename: old, new follow
			path = parts[i+2]
			i += 2
		}
		fc := FileChange{Path: path}
		if fields[0] == "-" {
			fc.Binary = true
		} else {
			fc.Added, _ = strconv.Atoi(fields[0])
			fc.Deleted, _ = strconv.Atoi(fields[1])
		}
		files = append(files, fc)
	}
	return files
}
