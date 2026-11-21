package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// coderClone is an agent's clone on its own branch, with one commit of its own.
func coderClone(t *testing.T, repo *testkit.Repo) string {
	t.Helper()
	path := newPath(t)
	if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/x", "main")); err != nil {
		t.Fatal(err)
	}
	commitInDir(t, path, "a.txt", "coder's first\n")
	return path
}

func commitInDir(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hardened(t, dir, "add", name)
	hardened(t, dir, "-c", "user.name=coder", "-c", "user.email=c@example.com", "commit", "-q", "-m", "add "+name)
	return hardened(t, dir, "rev-parse", "HEAD")
}

func TestAReviewCopyIsPinnedToTheCodersCommitAndResetEachRound(t *testing.T) {
	repo := testkit.NewRepo(t)
	coder := coderClone(t, repo)
	first := hardened(t, coder, "rev-parse", "HEAD")
	dest := newPath(t)

	sha, err := git.SnapshotForReview(ctx, repo.Dir, coder, "feat/x", "t1", dest)
	if err != nil || sha != first {
		t.Fatalf("snapshot = %s, %v; want %s", sha, err, first)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(got) != "coder's first\n" {
		t.Errorf("a.txt = %q", got)
	}
	if head := hardened(t, dest, "rev-parse", "HEAD"); head != first {
		t.Errorf("review HEAD = %s", head)
	}
	if ref := repo.Git("rev-parse", git.ReviewRef("t1")); ref != first {
		t.Errorf("hidden ref = %s, want %s", ref, first)
	}
	if status := repo.Git("status", "--porcelain"); status != "" {
		t.Errorf("the main repository's working tree changed: %q", status)
	}

	// The reviewer changes its copy; the coder commits again meanwhile.
	os.WriteFile(filepath.Join(dest, "a.txt"), []byte("planted defect\n"), 0o644)
	os.WriteFile(filepath.Join(dest, "planted.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dest, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch "+filepath.Join(dest, "..", "hook-ran")+"\n"), 0o755)
	if got, _ := os.ReadFile(filepath.Join(coder, "a.txt")); string(got) != "coder's first\n" {
		t.Errorf("the reviewer's change reached the coder: %q", got)
	}
	second := commitInDir(t, coder, "b.txt", "coder's second\n")

	sha, err = git.SnapshotForReview(ctx, repo.Dir, coder, "feat/x", "t1", dest)
	if err != nil || sha != second {
		t.Fatalf("second snapshot = %s, %v; want %s", sha, err, second)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(got) != "coder's first\n" {
		t.Errorf("a planted change survived the next round: %q", got)
	}
	for _, gone := range []string{"planted.txt", ".git/hooks/post-checkout"} {
		if _, err := os.Stat(filepath.Join(dest, gone)); err == nil {
			t.Errorf("%s survived the next round", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "..", "hook-ran")); err == nil {
		t.Error("a hook the reviewer planted ran")
	}
	if _, err := os.Stat(filepath.Join(dest, "b.txt")); err != nil {
		t.Error("the coder's new commit is not in the copy")
	}
}

func TestAReviewCopyRefusesALinkedDestination(t *testing.T) {
	repo := testkit.NewRepo(t)
	coder := coderClone(t, repo)
	elsewhere := t.TempDir()
	dest := filepath.Join(t.TempDir(), "review")
	os.Symlink(elsewhere, dest)
	if _, err := git.SnapshotForReview(ctx, repo.Dir, coder, "feat/x", "t1", dest); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("err = %v, want a refusal naming the link", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("wrote through the link: %v", entries)
	}
}

func TestRemovingAReviewDropsTheRefAndTheCopy(t *testing.T) {
	repo := testkit.NewRepo(t)
	coder := coderClone(t, repo)
	dest := newPath(t)
	if _, err := git.SnapshotForReview(ctx, repo.Dir, coder, "feat/x", "t1", dest); err != nil {
		t.Fatal(err)
	}
	if err := git.RemoveReview(ctx, repo.Dir, "t1", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("the copy is still there")
	}
	if out, err := git.Trusted().Run(ctx, repo.Dir, "for-each-ref", "refs/rig/"); err != nil || out != "" {
		t.Errorf("refs left: %q, %v", out, err)
	}
	if err := git.RemoveReview(ctx, repo.Dir, "t1", dest); err != nil {
		t.Errorf("removing twice: %v", err)
	}
}
