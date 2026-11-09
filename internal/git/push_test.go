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

// remoteFor gives repo a bare remote named origin that already has main.
func remoteFor(t *testing.T, repo *testkit.Repo) (bare string) {
	t.Helper()
	bare = filepath.Join(testkit.RealTempDir(t), "remote.git")
	testkit.RunGit(t, filepath.Dir(bare), "init", "-q", "--bare", "-b", "main", bare)
	repo.Git("remote", "add", "origin", bare)
	repo.Git("push", "-q", "origin", "main")
	return bare
}

func remoteTip(t *testing.T, bare, branch string) string {
	t.Helper()
	out, err := git.Trusted().Run(ctx, bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return out
}

func opts(repo *testkit.Repo, branch string) git.PushOptions {
	return git.PushOptions{Repo: repo.Dir, Branch: branch, Base: "main"}
}

// ticketWork provisions a clone, commits in it and imports it, as a ticket does before
// pushing. It returns the clone and the tip.
func ticketWork(t *testing.T, repo *testkit.Repo, branch string) (git.Checkout, string) {
	t.Helper()
	co := provision(t, repo, domain.CheckoutClone, branch)
	sha := commitIn(t, co, "feature.txt", "feature\n")
	if err := co.Import(ctx, false); err != nil {
		t.Fatal(err)
	}
	return co, sha
}

func TestACloneReachesTheRemoteOnlyThroughImportThenPush(t *testing.T) {
	repo := testkit.NewRepo(t)
	bare := remoteFor(t, repo)
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	sha := commitIn(t, co, "feature.txt", "feature\n")

	// before the import the main repository does not have the branch: nothing can be pushed
	if _, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x")); !errors.Is(err, git.ErrNotImported) || !strings.Contains(err.Error(), "import") {
		t.Errorf("push before import = %v, want ErrNotImported", err)
	}
	if remoteTip(t, bare, "feat/x") != "" {
		t.Fatal("the remote has the branch before the import")
	}

	if err := co.Import(ctx, false); err != nil {
		t.Fatal(err)
	}
	res, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x"))
	if err != nil || !res.Pushed || res.Sha != sha || res.Ref != "refs/heads/feat/x" || res.Remote != "origin" || res.URL != bare {
		t.Fatalf("Push = %+v, %v", res, err)
	}
	if remoteTip(t, bare, "feat/x") != sha {
		t.Error("the remote branch is not at the pushed commit")
	}

	// the clone's own origin is the main repository, not the real remote
	if url := testkit.RunGit(t, co.Path(), "remote", "get-url", "origin"); url == bare {
		t.Error("the clone can reach the real remote")
	}
}

func TestPushRunsFromTheMainRepositoryNotFromTheClone(t *testing.T) {
	repo := testkit.NewRepo(t)
	remoteFor(t, repo)
	co, _ := ticketWork(t, repo, "feat/x")
	script, marker := recorder(t)
	_ = script
	hook := filepath.Join(co.Path(), ".git", "hooks", "pre-push")
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	if _, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x")); err != nil {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Error("a hook in the clone ran during the push")
	}
}

func TestPushBranchUpdatesTheRemoteBranchAndNeverForces(t *testing.T) {
	repo := testkit.NewRepo(t)
	bare := remoteFor(t, repo)
	co, first := ticketWork(t, repo, "feat/x")
	if _, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x")); err != nil {
		t.Fatal(err)
	}

	second := commitIn(t, co, "more.txt", "more\n")
	co.Import(ctx, false)
	res, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x"))
	if err != nil || remoteTip(t, bare, "feat/x") != second || res.Sha != second {
		t.Fatalf("fast-forward push = %+v, %v; remote at %s, want %s", res, err, remoteTip(t, bare, "feat/x"), second)
	}

	again, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x"))
	if err != nil || !again.Pushed {
		t.Errorf("pushing what the remote already has = %+v, %v", again, err)
	}

	// someone else pushed to the same remote branch meanwhile
	other := filepath.Join(testkit.RealTempDir(t), "other")
	testkit.RunGit(t, filepath.Dir(other), "clone", "-q", bare, other)
	testkit.RunGit(t, other, "checkout", "-q", "feat/x")
	os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("theirs\n"), 0o644)
	testkit.RunGit(t, other, "add", "-A")
	testkit.RunGit(t, other, "-c", "user.name=o", "-c", "user.email=o@o", "commit", "-qm", "theirs")
	testkit.RunGit(t, other, "push", "-q", "origin", "feat/x")
	theirs := remoteTip(t, bare, "feat/x")

	third := commitIn(t, co, "third.txt", "third\n")
	co.Import(ctx, false)
	_, err = git.Push(ctx, domain.PushBranch, opts(repo, "feat/x"))
	if !errors.Is(err, git.ErrRejected) || !strings.Contains(err.Error(), "fast-forward") {
		t.Errorf("a non-fast-forward = %v, want ErrRejected", err)
	}
	if remoteTip(t, bare, "feat/x") != theirs || third == theirs {
		t.Error("the remote branch was overwritten")
	}
	_ = first
}

func TestMergeFastForwardsTheBase(t *testing.T) {
	repo := testkit.NewRepo(t)
	bare := remoteFor(t, repo)
	_, sha := ticketWork(t, repo, "feat/x")
	localMainBefore := repo.Git("rev-parse", "main")

	res, err := git.Push(ctx, domain.PushMerge, opts(repo, "feat/x"))
	if err != nil || !res.Pushed || res.Ref != "refs/heads/main" || res.Sha != sha {
		t.Fatalf("Push = %+v, %v", res, err)
	}
	if remoteTip(t, bare, "main") != sha {
		t.Error("the remote base did not fast-forward to the feature tip")
	}
	if remoteTip(t, bare, "feat/x") != "" {
		t.Error("merge mode must not publish the feature branch")
	}
	if repo.Git("rev-parse", "main") != localMainBefore {
		t.Error("the user's own main branch (possibly checked out) was moved")
	}
	if status := repo.Git("status", "--porcelain"); status != "" {
		t.Errorf("the main working tree was touched: %q", status)
	}

	again, err := git.Push(ctx, domain.PushMerge, opts(repo, "feat/x"))
	if err != nil || again.Pushed || !strings.Contains(again.Note, "already part of main") {
		t.Errorf("merging what is already merged = %+v, %v", again, err)
	}
}

func TestMergeRefusesADivergedBaseAndAllowsAMergeCommitWhenConfigured(t *testing.T) {
	repo := testkit.NewRepo(t)
	bare := remoteFor(t, repo)
	_, featureTip := ticketWork(t, repo, "feat/x")

	// the remote's main moves on with an unrelated file
	other := filepath.Join(testkit.RealTempDir(t), "other")
	testkit.RunGit(t, filepath.Dir(other), "clone", "-q", bare, other)
	os.WriteFile(filepath.Join(other, "unrelated.txt"), []byte("unrelated\n"), 0o644)
	testkit.RunGit(t, other, "add", "-A")
	testkit.RunGit(t, other, "-c", "user.name=o", "-c", "user.email=o@o", "commit", "-qm", "main moves on")
	testkit.RunGit(t, other, "push", "-q", "origin", "main")
	moved := remoteTip(t, bare, "main")

	_, err := git.Push(ctx, domain.PushMerge, opts(repo, "feat/x"))
	if !errors.Is(err, git.ErrDiverged) || !strings.Contains(err.Error(), "origin/main") || !strings.Contains(err.Error(), "merge commit") {
		t.Fatalf("diverged = %v, want ErrDiverged with advice", err)
	}
	if remoteTip(t, bare, "main") != moved {
		t.Fatal("the remote base changed although the merge was refused")
	}

	o := opts(repo, "feat/x")
	o.MergeCommit = true
	res, err := git.Push(ctx, domain.PushMerge, o)
	if err != nil || !res.Pushed {
		t.Fatalf("with a merge commit allowed: %+v, %v", res, err)
	}
	tip := remoteTip(t, bare, "main")
	parents := strings.Fields(testkit.RunGit(t, bare, "log", "-1", "--format=%P", tip))
	if len(parents) != 2 || parents[0] != moved || parents[1] != featureTip {
		t.Errorf("merge parents = %v, want [%s %s]", parents, moved, featureTip)
	}
	files := testkit.RunGit(t, bare, "ls-tree", "-r", "--name-only", tip)
	if !strings.Contains(files, "feature.txt") || !strings.Contains(files, "unrelated.txt") {
		t.Errorf("the merge lacks one side's work: %s", files)
	}
	if status := repo.Git("status", "--porcelain"); status != "" {
		t.Errorf("the main working tree was touched: %q", status)
	}
}

func TestAConflictingMergeIsRefusedAndPushesNothing(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Commit("shared.txt", "base\n", "add shared")
	bare := remoteFor(t, repo)
	co := provision(t, repo, domain.CheckoutClone, "feat/x")
	commitIn(t, co, "shared.txt", "ours\n")
	co.Import(ctx, false)

	other := filepath.Join(testkit.RealTempDir(t), "other")
	testkit.RunGit(t, filepath.Dir(other), "clone", "-q", bare, other)
	os.WriteFile(filepath.Join(other, "shared.txt"), []byte("theirs\n"), 0o644)
	testkit.RunGit(t, other, "add", "-A")
	testkit.RunGit(t, other, "-c", "user.name=o", "-c", "user.email=o@o", "commit", "-qm", "conflicting")
	testkit.RunGit(t, other, "push", "-q", "origin", "main")
	moved := remoteTip(t, bare, "main")

	o := opts(repo, "feat/x")
	o.MergeCommit = true
	_, err := git.Push(ctx, domain.PushMerge, o)
	if !errors.Is(err, git.ErrMergeConflict) || !strings.Contains(err.Error(), "shared.txt") {
		t.Errorf("error = %v, want a conflict naming shared.txt", err)
	}
	if remoteTip(t, bare, "main") != moved {
		t.Error("something was pushed despite the conflict")
	}
}

func TestPushModeNonePushesNothing(t *testing.T) {
	repo := testkit.NewRepo(t)
	bare := remoteFor(t, repo)
	_, sha := ticketWork(t, repo, "feat/x")
	res, err := git.Push(ctx, domain.PushNone, opts(repo, "feat/x"))
	if err != nil || res.Pushed || res.Sha != sha || !strings.Contains(res.Note, "nothing was pushed") {
		t.Errorf("Push = %+v, %v", res, err)
	}
	if remoteTip(t, bare, "feat/x") != "" || remoteTip(t, bare, "main") == sha {
		t.Error("push mode none pushed something")
	}
}

func TestRemoteAndModeErrors(t *testing.T) {
	t.Run("no remote at all", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		ticketWork(t, repo, "feat/x")
		_, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x"))
		if !errors.Is(err, git.ErrNoRemote) || !strings.Contains(err.Error(), "git remote add") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a remote that does not exist", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		remoteFor(t, repo)
		ticketWork(t, repo, "feat/x")
		o := opts(repo, "feat/x")
		o.Remote = "upstream"
		_, err := git.Push(ctx, domain.PushBranch, o)
		if !errors.Is(err, git.ErrNoRemote) || !strings.Contains(err.Error(), "origin") {
			t.Errorf("error = %v, want it to list the remotes that exist", err)
		}
	})
	t.Run("a remote without the base branch", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		remoteFor(t, repo)
		ticketWork(t, repo, "feat/x")
		o := opts(repo, "feat/x")
		o.Base = "release"
		if _, err := git.Push(ctx, domain.PushMerge, o); err == nil || !strings.Contains(err.Error(), "release") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("merge mode needs a base, and pr is not handled here", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		remoteFor(t, repo)
		ticketWork(t, repo, "feat/x")
		o := opts(repo, "feat/x")
		o.Base = ""
		if _, err := git.Push(ctx, domain.PushMerge, o); err == nil {
			t.Error("merge without a base was accepted")
		}
		if _, err := git.Push(ctx, domain.PushPR, opts(repo, "feat/x")); err == nil {
			t.Error("pr mode must not be handled by Push")
		}
	})
	t.Run("an unreachable remote", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		repo.Git("remote", "add", "origin", filepath.Join(t.TempDir(), "gone.git"))
		ticketWork(t, repo, "feat/x")
		if _, err := git.Push(ctx, domain.PushBranch, opts(repo, "feat/x")); err == nil {
			t.Error("pushing to a missing remote succeeded")
		}
	})
}
