package git_test

import (
	"slices"
	"testing"

	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestABaseThatMovedIsBehindAndConflictsAreNamed(t *testing.T) {
	repo := testkit.NewRepo(t)
	co, _ := ticketWork(t, repo, "feat/x") // the ticket adds its own file
	st, err := git.CompareBase(ctx, repo.Dir, co.Path(), "main")
	if err != nil || st.Behind != 0 || len(st.Conflicts) != 0 {
		t.Fatalf("fresh ticket: %+v, %v", st, err)
	}
	repo.Commit("other.txt", "theirs\n", "an unrelated change lands")
	if st, err = git.CompareBase(ctx, repo.Dir, co.Path(), "main"); err != nil || st.Behind != 1 || len(st.Conflicts) != 0 {
		t.Errorf("after an unrelated change: %+v, %v", st, err)
	}
	commitIn(t, co, "shared.txt", "ours\n")
	repo.Commit("shared.txt", "theirs\n", "the same file lands")
	if st, err = git.CompareBase(ctx, repo.Dir, co.Path(), "main"); err != nil || st.Behind != 2 || !slices.Equal(st.Conflicts, []string{"shared.txt"}) {
		t.Errorf("after a conflicting change: %+v, %v", st, err)
	}
	if _, err := git.CompareBase(ctx, repo.Dir, co.Path(), "nope"); err == nil {
		t.Error("a missing base was not an error")
	}
}

func TestMergingTheBaseCommitsOrLeavesTheConflictForTheAgent(t *testing.T) {
	repo := testkit.NewRepo(t)
	co, _ := ticketWork(t, repo, "feat/x")
	repo.Commit("other.txt", "theirs\n", "an unrelated change lands")
	sha, conflicts, err := git.MergeBase(ctx, repo.Dir, co.Path(), "main", "rig", "rig@localhost")
	if err != nil || sha == "" || len(conflicts) != 0 {
		t.Fatalf("clean merge: %q %v %v", sha, conflicts, err)
	}
	if st, _ := git.CompareBase(ctx, repo.Dir, co.Path(), "main"); st.Behind != 0 {
		t.Errorf("still behind after the merge: %+v", st)
	}

	commitIn(t, co, "shared.txt", "ours\n")
	repo.Commit("shared.txt", "theirs\n", "the same file lands")
	sha, conflicts, err = git.MergeBase(ctx, repo.Dir, co.Path(), "main", "rig", "rig@localhost")
	if err != nil || sha != "" || !slices.Equal(conflicts, []string{"shared.txt"}) {
		t.Errorf("conflicting merge: %q %v %v", sha, conflicts, err)
	}
	if out := testkit.RunGit(t, co.Path(), "rev-parse", "-q", "--verify", "MERGE_HEAD"); out == "" {
		t.Error("the merge is not left in progress for the agent")
	}
}
