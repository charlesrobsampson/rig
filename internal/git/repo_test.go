package git_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestBranchesListsLocalBranchesSorted(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Git("branch", "zeta")
	repo.Git("branch", "feat/alpha")
	got, err := git.Branches(ctx, repo.Dir)
	if want := []string{"feat/alpha", "main", "zeta"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Branches = %v, %v; want %v", got, err, want)
	}
}

func TestCurrentCommit(t *testing.T) {
	repo := testkit.NewRepo(t)
	first, err := git.CurrentCommit(ctx, repo.Dir)
	if err != nil || first != repo.Git("rev-parse", "HEAD") {
		t.Fatalf("CurrentCommit = %q, %v", first, err)
	}
	second := repo.Commit("a.txt", "x\n", "second")
	if got, _ := git.CurrentCommit(ctx, repo.Dir); got != second || got == first {
		t.Errorf("CurrentCommit after a new commit = %q, want %q", got, second)
	}
	if _, err := git.CurrentCommit(ctx, t.TempDir()); err == nil {
		t.Error("expected an error outside a repository")
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Run("origin/HEAD wins", func(t *testing.T) {
		origin := testkit.NewRepo(t)
		origin.Git("branch", "-m", "trunk")
		clone := filepath.Join(testkit.RealTempDir(t), "clone")
		testkit.RunGit(t, origin.Dir, "clone", "-q", origin.Dir, clone)
		// A local main would normally be preferred; origin/HEAD must take priority.
		testkit.RunGit(t, clone, "branch", "main")
		got, err := git.DefaultBranch(ctx, clone)
		if err != nil || got != "trunk" {
			t.Errorf("DefaultBranch = %q, %v; want trunk", got, err)
		}
	})

	t.Run("main", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		if got, err := git.DefaultBranch(ctx, repo.Dir); err != nil || got != "main" {
			t.Errorf("DefaultBranch = %q, %v", got, err)
		}
	})

	t.Run("master when there is no main", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		repo.Git("branch", "-m", "master")
		if got, err := git.DefaultBranch(ctx, repo.Dir); err != nil || got != "master" {
			t.Errorf("DefaultBranch = %q, %v", got, err)
		}
	})

	t.Run("an unrecognizable default asks for an explicit base", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		repo.Git("branch", "-m", "develop")
		_, err := git.DefaultBranch(ctx, repo.Dir)
		if err == nil || !strings.Contains(err.Error(), "base") {
			t.Errorf("error = %v, want one telling the user to set a base", err)
		}
	})
}
