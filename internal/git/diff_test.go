package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// baseRefFor is how a checkout of each kind names the base branch: a worktree shares
// the main repository's branches, a clone has its snapshot as origin/<base>.
func baseRefFor(k domain.CheckoutKind) string {
	if k == domain.CheckoutClone {
		return "origin/main"
	}
	return "main"
}

func filesByPath(res git.DiffResult) map[string]git.FileChange {
	out := map[string]git.FileChange{}
	for _, f := range res.Files {
		out[f.Path] = f
	}
	return out
}

func TestDiffShowsCommittedStagedUnstagedAndUntrackedWork(t *testing.T) {
	eachKind(t, func(t *testing.T, k kindUnderTest) {
		repo := testkit.NewRepo(t)
		repo.Commit("tracked.txt", "one\ntwo\nthree\n", "add tracked")
		repo.Commit("gone.txt", "bye\n", "add gone")
		co := provision(t, repo, k.kind, "feat/x")
		dir := co.Path()

		commitIn(t, co, "committed.txt", "c1\nc2\n")                                              // committed
		os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("one\nTWO\nthree\nfour\n"), 0o644) // unstaged edit
		os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("s1\n"), 0o644)
		testkit.RunGit(t, dir, "add", "staged.txt") // staged new file
		os.Remove(filepath.Join(dir, "gone.txt"))   // deletion
		os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u1\nu2\nu3\n"), 0o644)

		res, err := git.Diff(ctx, dir, baseRefFor(k.kind))
		if err != nil {
			t.Fatal(err)
		}
		files := filesByPath(res)
		want := map[string]git.FileChange{
			"committed.txt": {Path: "committed.txt", Added: 2},
			"tracked.txt":   {Path: "tracked.txt", Added: 2, Deleted: 1},
			"staged.txt":    {Path: "staged.txt", Added: 1},
			"gone.txt":      {Path: "gone.txt", Deleted: 1},
			"untracked.txt": {Path: "untracked.txt", Added: 3, Untracked: true},
		}
		if len(files) != len(want) {
			t.Errorf("files = %+v", res.Files)
		}
		for path, w := range want {
			if got := files[path]; got != w {
				t.Errorf("%s = %+v, want %+v", path, got, w)
			}
		}
		for _, want := range []string{"+c1", "+TWO", "-two", "+four", "+s1", "-bye", "+u1", "+u3", "diff --git a/tracked.txt b/tracked.txt", "new file mode"} {
			if !strings.Contains(res.Patch, want) {
				t.Errorf("the patch lacks %q", want)
			}
		}
		if res.MergeBase != testkit.RunGit(t, repo.Dir, "rev-parse", "main") && !k.safeWithoutImport {
			t.Errorf("merge base = %s", res.MergeBase)
		}
	})
}

func TestDiffOfACleanCheckoutIsEmpty(t *testing.T) {
	eachKind(t, func(t *testing.T, k kindUnderTest) {
		co := provision(t, testkit.NewRepo(t), k.kind, "feat/x")
		res, err := git.Diff(ctx, co.Path(), baseRefFor(k.kind))
		if err != nil || len(res.Files) != 0 || res.Patch != "" {
			t.Errorf("Diff = %+v, %v", res, err)
		}
	})
}

func TestDiffHandlesRenamesBinariesAndIgnoredFiles(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Commit("old-name.txt", "line1\nline2\nline3\nline4\nline5\n", "add")
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	dir := co.Path()
	testkit.RunGit(t, dir, "mv", "old-name.txt", "new name.txt") // a space in the path
	os.WriteFile(filepath.Join(dir, "image.bin"), []byte{0, 1, 2, 0, 255, 0, 0}, 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.log\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored.log"), []byte("noise\n"), 0o644)

	res, err := git.Diff(ctx, dir, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	files := filesByPath(res)
	if f, ok := files["new name.txt"]; !ok || f.Added != 0 || f.Deleted != 0 {
		t.Errorf("the rename = %+v in %+v", f, res.Files)
	}
	if _, stale := files["old-name.txt"]; stale {
		t.Errorf("a rename listed under its old name too: %+v", res.Files)
	}
	if f := files["image.bin"]; !f.Binary || !f.Untracked {
		t.Errorf("image.bin = %+v, want a binary untracked file", f)
	}
	if _, ok := files["ignored.log"]; ok || strings.Contains(res.Patch, "noise") {
		t.Error("an ignored file was shown")
	}
	if _, ok := files[".gitignore"]; !ok {
		t.Error(".gitignore itself is new work and should show")
	}
}

func TestAHugeUntrackedFileIsNotDumpedIntoTheReview(t *testing.T) {
	repo := testkit.NewRepo(t)
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	os.WriteFile(filepath.Join(co.Path(), "dataset.csv"), []byte(strings.Repeat("0123456789abcdef\n", 40000)), 0o644) // ~680 KB
	res, err := git.Diff(ctx, co.Path(), "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Patch) > 2000 || !strings.Contains(res.Patch, "dataset.csv is") || !strings.Contains(res.Patch, "not shown") {
		t.Errorf("patch = %.200q (%d bytes)", res.Patch, len(res.Patch))
	}
	if f := filesByPath(res)["dataset.csv"]; !f.Untracked {
		t.Errorf("the file should still be listed: %+v", res.Files)
	}
}

func TestDiffNeverChangesTheCheckout(t *testing.T) {
	repo := testkit.NewRepo(t)
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	dir := co.Path()
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x\n"), 0o644)
	before := testkit.RunGit(t, dir, "status", "--porcelain=v1")
	indexBefore, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))

	if _, err := git.Diff(ctx, dir, "origin/main"); err != nil {
		t.Fatal(err)
	}
	if after := testkit.RunGit(t, dir, "status", "--porcelain=v1"); after != before || !strings.Contains(after, "?? new.txt") {
		t.Errorf("status changed: %q -> %q (a new file must stay untracked, not marked intent-to-add)", before, after)
	}
	indexAfter, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if string(indexAfter) != string(indexBefore) {
		t.Error("the index was rewritten")
	}
}

func TestDiffRunsNothingThePlantedConfigNames(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Commit("a.txt", "original\n", "add a")
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	dir := co.Path()
	script, marker := recorder(t)
	testkit.RunGit(t, dir, "config", "core.fsmonitor", script)
	testkit.RunGit(t, dir, "config", "diff.external", script)
	testkit.RunGit(t, dir, "config", "diff.evil.textconv", script)
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt diff=evil\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("untracked\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)

	res, err := git.Diff(ctx, dir, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Error("a program planted in the clone ran while building the review diff")
	}
	if !strings.Contains(res.Patch, "+changed") || !strings.Contains(res.Patch, "+untracked") {
		t.Errorf("the diff itself must still be produced:\n%s", res.Patch)
	}
}

func TestDiffErrors(t *testing.T) {
	co := provision(t, testkit.NewRepo(t), domain.CheckoutClone, "feat/x")
	if _, err := git.Diff(ctx, co.Path(), "origin/no-such-branch"); err == nil || !strings.Contains(err.Error(), "origin/no-such-branch") {
		t.Errorf("an unknown base = %v", err)
	}
	if _, err := git.Diff(ctx, t.TempDir(), "main"); err == nil {
		t.Error("a directory that is not a repository should fail")
	}
}
