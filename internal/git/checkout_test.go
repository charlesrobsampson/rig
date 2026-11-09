package git_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func worktreeSpec(repo *testkit.Repo, path, branch, base string) git.Spec {
	return git.Spec{Repo: repo.Dir, Branch: branch, Base: base, Path: path, Kind: domain.CheckoutWorktree}
}

func newPath(t *testing.T) string { return filepath.Join(testkit.RealTempDir(t), "checkout") }

func head(t *testing.T, dir string) string {
	t.Helper()
	got, err := git.CurrentCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestProvisionWorktreeCreatesANewBranchFromTheBase(t *testing.T) {
	repo := testkit.NewRepo(t)
	baseCommit := repo.Git("rev-parse", "HEAD")
	repo.Git("checkout", "-q", "-b", "develop")
	repo.Commit("dev.txt", "dev\n", "on develop")
	developCommit := repo.Git("rev-parse", "HEAD")
	repo.Git("checkout", "-q", "main")

	tests := []struct {
		name     string
		base     string
		wantBase string
	}{
		{"the named base", "develop", developCommit},
		{"the repo's default branch when no base is given", "", baseCommit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := newPath(t)
			co, err := git.Provision(ctx, worktreeSpec(repo, path, "feat/"+strings.ReplaceAll(tt.name, " ", "-"), tt.base))
			if err != nil {
				t.Fatal(err)
			}
			if co.Kind() != domain.CheckoutWorktree || co.Path() != path || !strings.HasPrefix(co.Branch(), "feat/") {
				t.Errorf("checkout = %s %s %s", co.Kind(), co.Path(), co.Branch())
			}
			if got := head(t, path); got != tt.wantBase {
				t.Errorf("HEAD = %s, want the base commit %s", got, tt.wantBase)
			}
			if branch := testkit.RunGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); branch != co.Branch() {
				t.Errorf("checked out %q, want %q", branch, co.Branch())
			}
		})
	}
}

func TestProvisionWorktreeReusesAnExistingFreeBranchWithoutResettingIt(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Git("checkout", "-q", "-b", "feat/existing")
	existing := repo.Commit("f.txt", "work in progress\n", "wip")
	repo.Git("checkout", "-q", "main")

	path := newPath(t)
	co, err := git.Provision(ctx, worktreeSpec(repo, path, "feat/existing", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if got := head(t, path); got != existing {
		t.Errorf("HEAD = %s, want the branch's own commit %s (not the base)", got, existing)
	}
	if co.Branch() != "feat/existing" {
		t.Errorf("branch = %q", co.Branch())
	}
}

func TestProvisionWorktreeRefusesABranchCheckedOutElsewhere(t *testing.T) {
	t.Run("in the main working tree", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		_, err := git.Provision(ctx, worktreeSpec(repo, newPath(t), "main", "main"))
		if err == nil || !strings.Contains(err.Error(), "already checked out") || !strings.Contains(err.Error(), repo.Dir) || !errors.Is(err, git.ErrBranchInUse) {
			t.Errorf("error = %v, want ErrBranchInUse saying where the branch is checked out", err)
		}
	})

	t.Run("in another worktree", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		first := newPath(t)
		if _, err := git.Provision(ctx, worktreeSpec(repo, first, "feat/x", "main")); err != nil {
			t.Fatal(err)
		}
		_, err := git.Provision(ctx, worktreeSpec(repo, newPath(t), "feat/x", "main"))
		if err == nil || !strings.Contains(err.Error(), "already checked out") || !strings.Contains(err.Error(), first) {
			t.Errorf("error = %v", err)
		}
	})
}

func TestProvisionWorktreePathRules(t *testing.T) {
	repo := testkit.NewRepo(t)

	t.Run("a non-empty directory is refused", func(t *testing.T) {
		path := newPath(t)
		os.MkdirAll(path, 0o755)
		os.WriteFile(filepath.Join(path, "keep.txt"), []byte("mine"), 0o644)
		_, err := git.Provision(ctx, worktreeSpec(repo, path, "feat/a", "main"))
		if err == nil || !strings.Contains(err.Error(), "not empty") || !errors.Is(err, git.ErrPathInUse) {
			t.Errorf("error = %v, want ErrPathInUse", err)
		}
		if got, _ := os.ReadFile(filepath.Join(path, "keep.txt")); string(got) != "mine" {
			t.Error("existing files were touched")
		}
		if branches, _ := git.Branches(ctx, repo.Dir); contains(branches, "feat/a") {
			t.Error("a failed provision left its branch behind")
		}
	})

	t.Run("an existing empty directory is fine", func(t *testing.T) {
		path := newPath(t)
		os.MkdirAll(path, 0o755)
		if _, err := git.Provision(ctx, worktreeSpec(repo, path, "feat/b", "main")); err != nil {
			t.Errorf("empty directory rejected: %v", err)
		}
	})

	t.Run("a file in the way is refused", func(t *testing.T) {
		path := newPath(t)
		os.WriteFile(path, []byte("x"), 0o644)
		if _, err := git.Provision(ctx, worktreeSpec(repo, path, "feat/c", "main")); err == nil {
			t.Error("expected an error")
		}
	})
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func TestProvisionValidatesItsSpec(t *testing.T) {
	repo := testkit.NewRepo(t)
	good := worktreeSpec(repo, newPath(t), "feat/ok", "main")
	tests := []struct {
		name   string
		mutate func(*git.Spec)
		want   string
	}{
		{"relative path", func(s *git.Spec) { s.Path = "relative/path" }, "absolute"},
		{"relative repo", func(s *git.Spec) { s.Repo = "relative/repo" }, "absolute"},
		{"not a repository", func(s *git.Spec) { s.Repo = t.TempDir() }, "not a git repository"},
		{"missing branch", func(s *git.Spec) { s.Branch = "" }, "branch"},
		{"invalid branch name", func(s *git.Spec) { s.Branch = "bad..name" }, "bad..name"},
		{"unknown base", func(s *git.Spec) { s.Base = "no-such-base" }, "no-such-base"},
		{"missing kind", func(s *git.Spec) { s.Kind = "" }, "kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := good
			spec.Path = newPath(t)
			tt.mutate(&spec)
			_, err := git.Provision(ctx, spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
	if branches, _ := git.Branches(ctx, repo.Dir); contains(branches, "feat/ok") {
		t.Error("a rejected spec left a branch behind")
	}
}

func TestWorktreesOfOneRepoShareItsObjectStore(t *testing.T) {
	repo := testkit.NewRepo(t)
	a, b := newPath(t), newPath(t)
	if _, err := git.Provision(ctx, worktreeSpec(repo, a, "feat/a", "main")); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Provision(ctx, worktreeSpec(repo, b, "feat/b", "main")); err != nil {
		t.Fatal(err)
	}

	commonA := testkit.RunGit(t, a, "rev-parse", "--path-format=absolute", "--git-common-dir")
	commonB := testkit.RunGit(t, b, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if commonA != commonB || commonA != repo.Path(".git") {
		t.Errorf("common dirs %q and %q, want both to be the main repo's .git", commonA, commonB)
	}
	if info, err := os.Stat(filepath.Join(a, ".git")); err != nil || info.IsDir() {
		t.Errorf(".git in a worktree should be a pointer file, not a copy of the repository: %v", err)
	}

	// A commit made in one worktree is immediately visible from the other.
	os.WriteFile(filepath.Join(a, "shared.txt"), []byte("hi\n"), 0o644)
	testkit.RunGit(t, a, "add", "shared.txt")
	testkit.RunGit(t, a, "commit", "-q", "-m", "from a")
	sha := testkit.RunGit(t, a, "rev-parse", "HEAD")
	if typ := testkit.RunGit(t, b, "cat-file", "-t", sha); typ != "commit" {
		t.Errorf("the other worktree cannot see the new commit: %q", typ)
	}
}
