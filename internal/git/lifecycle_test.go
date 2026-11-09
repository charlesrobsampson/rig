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

// Both checkout kinds promise the same thing: work is never lost silently. The suite
// runs against each; "safeWithoutImport" records the one real difference, that a
// worktree's commits already live in the main repository.
type kindUnderTest struct {
	kind              domain.CheckoutKind
	safeWithoutImport bool
}

var kinds = []kindUnderTest{
	{domain.CheckoutWorktree, true},
	{domain.CheckoutClone, false},
}

func provision(t *testing.T, repo *testkit.Repo, k domain.CheckoutKind, branch string) git.Checkout {
	t.Helper()
	co, err := git.Provision(ctx, git.Spec{Repo: repo.Dir, Branch: branch, Base: "main", Path: newPath(t), Kind: k})
	if err != nil {
		t.Fatal(err)
	}
	return co
}

func commitIn(t *testing.T, co git.Checkout, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(co.Path(), file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.RunGit(t, co.Path(), "add", file)
	testkit.RunGit(t, co.Path(), "commit", "-q", "-m", "work on "+file)
	return testkit.RunGit(t, co.Path(), "rev-parse", "HEAD")
}

func mainBranchTip(t *testing.T, repo *testkit.Repo, branch string) string {
	t.Helper()
	out, err := git.Trusted().Run(ctx, repo.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return out
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func eachKind(t *testing.T, fn func(t *testing.T, k kindUnderTest)) {
	for _, k := range kinds {
		t.Run(string(k.kind), func(t *testing.T) { fn(t, k) })
	}
}

func TestACleanCheckoutReportsCleanAndRemoves(t *testing.T) {
	eachKind(t, func(t *testing.T, k kindUnderTest) {
		repo := testkit.NewRepo(t)
		co := provision(t, repo, k.kind, "feat/x")

		d, err := co.IsDirty(ctx)
		if err != nil || d != (git.Dirtiness{}) || d.Any() {
			t.Fatalf("IsDirty = %+v, %v; want a clean checkout", d, err)
		}
		if err := co.Remove(ctx, false); err != nil {
			t.Fatal(err)
		}
		if !gone(co.Path()) {
			t.Error("the checkout directory remains")
		}
		if k.kind == domain.CheckoutWorktree {
			if out := repo.Git("worktree", "list"); strings.Contains(out, co.Path()) {
				t.Errorf("the main repo still lists the removed worktree:\n%s", out)
			}
			if !contains(mustBranches(t, repo), "feat/x") {
				t.Error("removing a worktree must keep its branch")
			}
		}
	})
}

func mustBranches(t *testing.T, repo *testkit.Repo) []string {
	t.Helper()
	b, err := git.Branches(ctx, repo.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUncommittedWorkIsReportedAndProtected(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, co git.Checkout)
		want  git.Dirtiness
	}{
		{"a modified tracked file", func(t *testing.T, co git.Checkout) {
			os.WriteFile(filepath.Join(co.Path(), "README.md"), []byte("edited\n"), 0o644)
		}, git.Dirtiness{Modified: 1}},
		{"an untracked file", func(t *testing.T, co git.Checkout) {
			os.WriteFile(filepath.Join(co.Path(), "scratch.txt"), []byte("new\n"), 0o644)
		}, git.Dirtiness{Untracked: 1}},
		{"a staged new file", func(t *testing.T, co git.Checkout) {
			os.WriteFile(filepath.Join(co.Path(), "staged.txt"), []byte("new\n"), 0o644)
			testkit.RunGit(t, co.Path(), "add", "staged.txt")
		}, git.Dirtiness{Modified: 1}},
		{"a deleted tracked file", func(t *testing.T, co git.Checkout) {
			os.Remove(filepath.Join(co.Path(), "README.md"))
		}, git.Dirtiness{Modified: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachKind(t, func(t *testing.T, k kindUnderTest) {
				repo := testkit.NewRepo(t)
				co := provision(t, repo, k.kind, "feat/x")
				tt.setup(t, co)

				d, err := co.IsDirty(ctx)
				if err != nil || d != tt.want {
					t.Fatalf("IsDirty = %+v, %v; want %+v", d, err, tt.want)
				}

				err = co.Remove(ctx, false)
				var dirty *git.DirtyError
				if !errors.As(err, &dirty) || dirty.Path != co.Path() || dirty.Dirtiness != tt.want {
					t.Fatalf("Remove = %v, want a *git.DirtyError describing the work at risk", err)
				}
				if gone(co.Path()) {
					t.Fatal("a refused Remove deleted the checkout")
				}

				if err := co.Remove(ctx, true); err != nil {
					t.Fatalf("forced Remove: %v", err)
				}
				if !gone(co.Path()) {
					t.Error("a forced Remove left the directory")
				}
			})
		})
	}
}

func TestCommittedWorkIsNeverLostSilently(t *testing.T) {
	eachKind(t, func(t *testing.T, k kindUnderTest) {
		repo := testkit.NewRepo(t)
		co := provision(t, repo, k.kind, "feat/x")
		sha := commitIn(t, co, "work.txt", "done\n")

		d, err := co.IsDirty(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantUnimported := 1
		if k.safeWithoutImport {
			wantUnimported = 0
		}
		if d.Unimported != wantUnimported || d.Modified != 0 || d.Untracked != 0 {
			t.Errorf("IsDirty = %+v, want %d unimported commits and nothing uncommitted", d, wantUnimported)
		}

		if k.safeWithoutImport {
			if tip := mainBranchTip(t, repo, "feat/x"); tip != sha {
				t.Errorf("a worktree's commit should already be on the main repo's branch: %q vs %q", tip, sha)
			}
		} else {
			err := co.Remove(ctx, false)
			var dirty *git.DirtyError
			if !errors.As(err, &dirty) || dirty.Unimported != 1 || !strings.Contains(err.Error(), "import") {
				t.Fatalf("Remove = %v; a clone with unimported commits must refuse and mention importing", err)
			}
			if gone(co.Path()) {
				t.Fatal("the refused Remove deleted the clone")
			}
			if mainBranchTip(t, repo, "feat/x") != "" {
				t.Fatal("the branch reached the main repo before Import")
			}
		}

		if err := co.Import(ctx, false); err != nil {
			t.Fatal(err)
		}
		if tip := mainBranchTip(t, repo, "feat/x"); tip != sha {
			t.Errorf("after Import the main repo's branch is at %q, want %q", tip, sha)
		}
		if d, _ := co.IsDirty(ctx); d.Any() {
			t.Errorf("IsDirty after Import = %+v, want clean", d)
		}
		if err := co.Remove(ctx, false); err != nil {
			t.Fatalf("Remove after Import: %v", err)
		}
		if tip := mainBranchTip(t, repo, "feat/x"); tip != sha {
			t.Error("removing the checkout lost the branch")
		}
	})
}

func TestImportIsRepeatableAndPicksUpNewCommits(t *testing.T) {
	eachKind(t, func(t *testing.T, k kindUnderTest) {
		repo := testkit.NewRepo(t)
		co := provision(t, repo, k.kind, "feat/x")
		commitIn(t, co, "one.txt", "1\n")
		for i := 0; i < 2; i++ {
			if err := co.Import(ctx, false); err != nil {
				t.Fatalf("Import #%d: %v", i+1, err)
			}
		}
		second := commitIn(t, co, "two.txt", "2\n")
		if err := co.Import(ctx, false); err != nil {
			t.Fatal(err)
		}
		if tip := mainBranchTip(t, repo, "feat/x"); tip != second {
			t.Errorf("main branch at %q, want the newest commit %q", tip, second)
		}
	})
}

func TestImportRefusesToRewriteTheMainBranchUnlessForced(t *testing.T) {
	repo := testkit.NewRepo(t)
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	sha := commitIn(t, co, "clone.txt", "from the clone\n")

	// Meanwhile the main repository's branch moved on without the clone's commit.
	repo.Git("checkout", "-q", "-b", "feat/x", "main")
	diverged := repo.Commit("main-side.txt", "main moved\n", "diverge")
	repo.Git("checkout", "-q", "main")

	err := co.Import(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatalf("Import = %v, want a refusal that names the non-fast-forward", err)
	}
	if tip := mainBranchTip(t, repo, "feat/x"); tip != diverged {
		t.Errorf("the refused import changed the main branch to %q", tip)
	}
	if d, _ := co.IsDirty(ctx); d.Unimported != 1 {
		t.Errorf("IsDirty = %+v; the clone's commit is still unimported", d)
	}
	if err := co.Remove(ctx, false); err == nil {
		t.Error("Remove must still refuse while the commit is unimported")
	}

	if err := co.Import(ctx, true); err != nil {
		t.Fatalf("forced Import: %v", err)
	}
	if tip := mainBranchTip(t, repo, "feat/x"); tip != sha {
		t.Errorf("forced import left the branch at %q, want %q", tip, sha)
	}
}

func TestImportExplainsABranchCheckedOutInTheMainRepo(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Git("checkout", "-q", "-b", "feat/x")
	repo.Git("checkout", "-q", "main")
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	commitIn(t, co, "work.txt", "x\n")
	repo.Git("checkout", "-q", "feat/x") // the user switched to it

	err := co.Import(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "checked out") || !strings.Contains(err.Error(), repo.Dir) {
		t.Errorf("Import = %v, want one saying where the branch is checked out", err)
	}
}

func TestImportFailsClearlyWhenTheBranchIsGoneFromTheClone(t *testing.T) {
	repo := testkit.NewRepo(t)
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	testkit.RunGit(t, co.Path(), "checkout", "-q", "--detach")
	testkit.RunGit(t, co.Path(), "branch", "-q", "-D", "feat/x")
	if err := co.Import(ctx, false); err == nil || !strings.Contains(err.Error(), "feat/x") {
		t.Errorf("Import = %v, want one naming the missing branch", err)
	}
}

func TestRemoveOfAMissingCheckoutIsANoOp(t *testing.T) {
	eachKind(t, func(t *testing.T, k kindUnderTest) {
		repo := testkit.NewRepo(t)
		co := provision(t, repo, k.kind, "feat/x")
		if err := co.Remove(ctx, false); err != nil {
			t.Fatal(err)
		}
		if err := co.Remove(ctx, false); err != nil {
			t.Errorf("removing twice: %v", err)
		}
	})
}

func TestHostileCloneConfigCannotRunCodeDuringDirtyChecksOrRemoval(t *testing.T) {
	repo := testkit.NewRepo(t)
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	script, marker := recorder(t)
	testkit.RunGit(t, co.Path(), "config", "core.fsmonitor", script)
	os.WriteFile(filepath.Join(co.Path(), "a.txt"), []byte("x\n"), 0o644)
	hook := filepath.Join(co.Path(), ".git", "hooks", "post-checkout")
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)

	if _, err := co.IsDirty(ctx); err != nil {
		t.Fatal(err)
	}
	co.Import(ctx, false)
	co.Remove(ctx, true)
	if exists(marker) {
		t.Error("a program planted in the clone ran on the host")
	}
}

func TestOpenReattachesToAnExistingCheckout(t *testing.T) {
	eachKind(t, func(t *testing.T, k kindUnderTest) {
		repo := testkit.NewRepo(t)
		original := provision(t, repo, k.kind, "feat/x")
		sha := commitIn(t, original, "work.txt", "x\n")

		reopened, err := git.Open(ctx, git.Spec{Repo: repo.Dir, Branch: "feat/x", Path: original.Path(), Kind: k.kind})
		if err != nil {
			t.Fatal(err)
		}
		if reopened.Kind() != k.kind || reopened.Path() != original.Path() || reopened.Branch() != "feat/x" {
			t.Errorf("reopened = %s %s %s", reopened.Kind(), reopened.Path(), reopened.Branch())
		}
		if err := reopened.Import(ctx, false); err != nil {
			t.Fatal(err)
		}
		if mainBranchTip(t, repo, "feat/x") != sha {
			t.Error("a reopened checkout did not import")
		}
	})

	t.Run("the wrong kind or a missing directory is refused", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		wt := provision(t, repo, domain.CheckoutWorktree, "feat/w")
		spec := git.Spec{Repo: repo.Dir, Branch: "feat/w", Path: wt.Path(), Kind: domain.CheckoutClone}
		if _, err := git.Open(ctx, spec); err == nil || !strings.Contains(err.Error(), "clone") {
			t.Errorf("opening a worktree as a clone = %v", err)
		}
		spec.Path = filepath.Join(t.TempDir(), "missing")
		if _, err := git.Open(ctx, spec); err == nil {
			t.Error("opening a missing directory should fail")
		}
	})
}
