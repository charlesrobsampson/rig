package git_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestTouchedListsCommittedAndUncommittedFiles(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Commit("staged.txt", "a\n", "base file")
	repo.Commit("unstaged.txt", "a\n", "base file")
	co := provision(t, repo, domain.CheckoutClone, "feat/touched")
	if got, err := git.Touched(ctx, co.Path(), "main"); err != nil || len(got) != 0 {
		t.Fatalf("fresh checkout: %v, %v", got, err)
	}
	commitIn(t, co, "committed.txt", "one\n")
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(co.Path(), name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(co.Path(), name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("staged.txt", "b\n")
	testkit.RunGit(t, co.Path(), "add", "staged.txt")
	write("unstaged.txt", "b\n")
	write("sub/new.txt", "new\n")
	write(".gitignore", "ignored.log\n")
	testkit.RunGit(t, co.Path(), "add", ".gitignore")
	write("ignored.log", "x\n")

	got, err := git.Touched(ctx, co.Path(), "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "committed.txt", "staged.txt", "sub/new.txt", "unstaged.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("touched = %v, want %v", got, want)
	}
}

func TestTouchedCountsBothSidesOfARename(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Commit("old.txt", "some content that git will see as the same file\n", "add old")
	co := provision(t, repo, domain.CheckoutClone, "feat/rename")
	testkit.RunGit(t, co.Path(), "mv", "old.txt", "new.txt")
	got, err := git.Touched(ctx, co.Path(), "main")
	if err != nil || !slices.Equal(got, []string{"new.txt", "old.txt"}) {
		t.Errorf("touched = %v, %v", got, err)
	}
}

func TestTouchedWithAMissingBaseIsAnError(t *testing.T) {
	repo := testkit.NewRepo(t)
	co := provision(t, repo, domain.CheckoutClone, "feat/nobase")
	if _, err := git.Touched(ctx, co.Path(), "nope"); err == nil {
		t.Error("a missing base was not an error")
	}
}
