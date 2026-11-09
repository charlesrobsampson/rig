package git_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func prOpts(repo *testkit.Repo, branch string) git.PROptions {
	return git.PROptions{PushOptions: opts(repo, branch), Title: "Add the feature", Body: "the body"}
}

func TestOpeningAPullRequestPushesTheBranchAndCreatesItWithTheTitleAndBody(t *testing.T) {
	repo := testkit.NewRepo(t)
	bare := remoteFor(t, repo)
	_, sha := ticketWork(t, repo, "feat/x")
	gh := testkit.NewFakeGH()

	res, err := git.PushPR(ctx, gh, prOpts(repo, "feat/x"))
	if err != nil || !res.Pushed || res.Mode != domain.PushPR || res.PRURL == "" || res.Sha != sha {
		t.Fatalf("PushPR = %+v, %v", res, err)
	}
	if remoteTip(t, bare, "feat/x") != sha {
		t.Error("the branch was not pushed before the PR was opened")
	}
	prs := gh.PRs()
	if len(prs) != 1 {
		t.Fatalf("PRs = %+v", prs)
	}
	pr := prs[0]
	if pr.Head != "feat/x" || pr.Base != "main" || pr.Title != "Add the feature" || pr.Body != "the body" || pr.Draft || pr.URL != res.PRURL {
		t.Errorf("PR = %+v", pr)
	}
	// gh runs in the main repository, where its remote and the user's login are
	if pr.Dir != repo.Dir {
		t.Errorf("gh ran in %s, want the main repository %s", pr.Dir, repo.Dir)
	}

	draft := prOpts(repo, "feat/x")
	gh2 := testkit.NewFakeGH()
	draft.Draft = true
	git.PushPR(ctx, gh2, draft)
	if !gh2.PRs()[0].Draft {
		t.Error("--draft was not passed")
	}
}

func TestAnOpenPullRequestIsReusedAndANewOneFollowsAClosedOne(t *testing.T) {
	repo := testkit.NewRepo(t)
	bare := remoteFor(t, repo)
	co, first := ticketWork(t, repo, "feat/x")
	gh := testkit.NewFakeGH()
	one, err := git.PushPR(ctx, gh, prOpts(repo, "feat/x"))
	if err != nil {
		t.Fatal(err)
	}

	second := commitIn(t, co, "more.txt", "more\n")
	co.Import(ctx, false)
	again, err := git.PushPR(ctx, gh, prOpts(repo, "feat/x"))
	if err != nil || again.PRURL != one.PRURL || len(gh.PRs()) != 1 || !strings.Contains(again.Note, "already open") {
		t.Fatalf("second PushPR = %+v, %v; PRs %+v", again, err, gh.PRs())
	}
	if remoteTip(t, bare, "feat/x") != second {
		t.Error("the new commits were not pushed to the existing PR's branch")
	}

	gh.Close(one.PRURL)
	third, err := git.PushPR(ctx, gh, prOpts(repo, "feat/x"))
	if err != nil || third.PRURL == one.PRURL || len(gh.PRs()) != 2 {
		t.Errorf("after the PR was closed: %+v, %v", third, err)
	}
	_ = first
}

func TestAMissingOrUnauthenticatedGHFailsClearlyBeforeAnythingIsPushed(t *testing.T) {
	tests := []struct {
		name string
		gh   func() *testkit.FakeGH
		want error
		hint string
	}{
		{"gh is not installed", func() *testkit.FakeGH { g := testkit.NewFakeGH(); g.Missing = true; return g }, git.ErrGHMissing, "https://cli.github.com"},
		{"gh is not logged in", func() *testkit.FakeGH { g := testkit.NewFakeGH(); g.Unauthenticated = true; return g }, git.ErrGHAuth, "gh auth login"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testkit.NewRepo(t)
			bare := remoteFor(t, repo)
			ticketWork(t, repo, "feat/x")
			_, err := git.PushPR(ctx, tt.gh(), prOpts(repo, "feat/x"))
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.hint) {
				t.Errorf("error = %v, want %v with %q", err, tt.want, tt.hint)
			}
			if remoteTip(t, bare, "feat/x") != "" {
				t.Error("the branch was pushed although a pull request could not be opened")
			}
		})
	}
}

func TestPullRequestPreconditions(t *testing.T) {
	t.Run("a branch that was not imported", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		remoteFor(t, repo)
		gh := testkit.NewFakeGH()
		if _, err := git.PushPR(ctx, gh, prOpts(repo, "feat/none")); !errors.Is(err, git.ErrNotImported) {
			t.Errorf("error = %v", err)
		}
		if len(gh.Calls()) != 0 {
			t.Errorf("gh was called: %v", gh.Calls())
		}
	})
	t.Run("no base branch", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		remoteFor(t, repo)
		ticketWork(t, repo, "feat/x")
		o := prOpts(repo, "feat/x")
		o.Base = ""
		if _, err := git.PushPR(ctx, testkit.NewFakeGH(), o); err == nil {
			t.Error("a PR without a base was accepted")
		}
	})
	t.Run("a rejected push opens no PR", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		bare := remoteFor(t, repo)
		co, _ := ticketWork(t, repo, "feat/x")
		gh := testkit.NewFakeGH()
		git.PushPR(ctx, gh, prOpts(repo, "feat/x"))

		other := filepath.Join(testkit.RealTempDir(t), "other")
		testkit.RunGit(t, filepath.Dir(other), "clone", "-q", bare, other)
		testkit.RunGit(t, other, "checkout", "-q", "feat/x")
		os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("theirs\n"), 0o644)
		testkit.RunGit(t, other, "add", "-A")
		testkit.RunGit(t, other, "-c", "user.name=o", "-c", "user.email=o@o", "commit", "-qm", "theirs")
		testkit.RunGit(t, other, "push", "-q", "origin", "feat/x")

		commitIn(t, co, "mine.txt", "mine\n")
		co.Import(ctx, false)
		gh.Close(gh.PRs()[0].URL)
		_, err := git.PushPR(ctx, gh, prOpts(repo, "feat/x"))
		if !errors.Is(err, git.ErrRejected) || len(gh.PRs()) != 1 {
			t.Errorf("error = %v, PRs %d; the PR must not be opened for a branch that could not be pushed", err, len(gh.PRs()))
		}
	})
}

func TestGHFailuresAreReportedWithGHsOwnWords(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeFail)
		want   string
	}{
		{"pr create fails", func(f *fakeFail) {
			f.create = git.GHResult{ExitCode: 1, Stderr: "GraphQL: No commits between main and feat/x (createPullRequest)\n"}
		}, "No commits between"},
		{"pr create prints no URL", func(f *fakeFail) { f.create = git.GHResult{Stdout: "something odd\n"} }, "no URL"},
		{"pr list fails", func(f *fakeFail) { f.list = git.GHResult{ExitCode: 1, Stderr: "HTTP 401: Bad credentials\n"} }, "Bad credentials"},
		{"pr list prints garbage", func(f *fakeFail) { f.list = git.GHResult{Stdout: "not json"} }, "cannot read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testkit.NewRepo(t)
			remoteFor(t, repo)
			ticketWork(t, repo, "feat/x")
			f := &fakeFail{list: git.GHResult{Stdout: "[]"}, create: git.GHResult{Stdout: "https://github.com/a/b/pull/1\n"}}
			tt.mutate(f)
			_, err := git.PushPR(ctx, f, prOpts(repo, "feat/x"))
			if !errors.Is(err, git.ErrPR) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want ErrPR mentioning %q", err, tt.want)
			}
		})
	}
}

// fakeFail answers gh's version and auth checks, and returns scripted results for the rest.
type fakeFail struct {
	list, create git.GHResult
}

func (f *fakeFail) Run(_ context.Context, _ string, _ string, args ...string) (git.GHResult, error) {
	switch {
	case args[0] == "--version", args[0] == "auth":
		return git.GHResult{}, nil
	case args[0] == "pr" && args[1] == "list":
		return f.list, nil
	}
	return f.create, nil
}

func TestEachServiceOfATicketGetsItsOwnPullRequest(t *testing.T) {
	gh := testkit.NewFakeGH()
	var urls []string
	for _, svc := range []string{"api", "web"} {
		repo := testkit.NewRepo(t)
		remoteFor(t, repo)
		ticketWork(t, repo, "rig/ticket/"+svc)
		o := prOpts(repo, "rig/ticket/"+svc)
		o.Title = git.PRTitle(git.PRContent{Title: "Add dashboard", Service: svc, Multi: true})
		res, err := git.PushPR(ctx, gh, o)
		if err != nil {
			t.Fatal(err)
		}
		urls = append(urls, res.PRURL)
	}
	prs := gh.PRs()
	if len(prs) != 2 || urls[0] == urls[1] || prs[0].Title != "Add dashboard (api)" || prs[1].Title != "Add dashboard (web)" || prs[0].Dir == prs[1].Dir {
		t.Errorf("PRs = %+v", prs)
	}
}

func TestPRBody(t *testing.T) {
	files := []git.FileChange{{Path: "main.go", Added: 10, Deleted: 2}, {Path: "docs/new.md", Added: 5}}
	body := git.PRBody(git.PRContent{TicketID: "proj-412", Title: "Add dashboard", Source: "jira PROJ-412", Plan: "# Plan: Add dashboard\n\nUse a locale header.\n\n- step one\n", Files: files})
	for _, want := range []string{"Ticket `proj-412` (jira PROJ-412)", "## Plan", "Use a locale header.", "- step one", "## Changes", "| `main.go` | 10 | 2 |", "| `docs/new.md` | 5 | 0 |", "2 files changed, +15 -2", "Opened by [rig]"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "# Plan: Add dashboard") {
		t.Error("the plan's own title just repeats the PR title and should be dropped")
	}

	multi := git.PRBody(git.PRContent{TicketID: "t", Service: "api", Multi: true})
	if !strings.Contains(multi, "service `api`") || strings.Contains(multi, "## Plan") || strings.Contains(multi, "## Changes") {
		t.Errorf("a body with no plan and no files:\n%s", multi)
	}
	if one := git.PRBody(git.PRContent{TicketID: "t", Files: files[:1]}); !strings.Contains(one, "1 file changed") {
		t.Errorf("singular:\n%s", one)
	}
	long := git.PRBody(git.PRContent{TicketID: "t", Plan: strings.Repeat("x", 50000)})
	if len(long) > 25000 || !strings.Contains(long, "plan truncated") {
		t.Errorf("a long plan made a %d-byte body", len(long))
	}
	if git.PRTitle(git.PRContent{Title: "T", Service: "api"}) != "T" || git.PRTitle(git.PRContent{Title: "T", Service: "api", Multi: true}) != "T (api)" {
		t.Error("PRTitle")
	}
}

func TestTheRealGHRunnerPassesArgumentsStdinAndTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	os.WriteFile(script, []byte(`#!/bin/sh
echo "args: $*"
echo "stdin: $(cat)"
echo "prompt: $GH_PROMPT_DISABLED cwd: $(pwd)"
echo "oops" >&2
exit 3
`), 0o755)
	res, err := git.NewGH(script).Run(ctx, dir, "body text", "pr", "create", "--title", "A B")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.Contains(res.Stdout, "args: pr create --title A B") || !strings.Contains(res.Stdout, "stdin: body text") || !strings.Contains(res.Stdout, "prompt: 1") || res.Stderr != "oops\n" {
		t.Errorf("result = %+v", res)
	}
	if _, err := git.NewGH(filepath.Join(dir, "no-such-gh")).Run(ctx, dir, "", "--version"); !errors.Is(err, git.ErrGHMissing) {
		t.Errorf("a missing binary = %v, want ErrGHMissing", err)
	}
}
