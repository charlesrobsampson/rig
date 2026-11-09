package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// pushRepo is a repository with a bare "origin" that already has main.
func pushRepo(t *testing.T) (*testkit.Repo, string) {
	t.Helper()
	repo := testkit.NewRepo(t)
	bare := filepath.Join(testkit.RealTempDir(t), "remote.git")
	testkit.RunGit(t, filepath.Dir(bare), "init", "-q", "--bare", "-b", "main", bare)
	repo.Git("remote", "add", "origin", bare)
	repo.Git("push", "-q", "origin", "main")
	return repo, bare
}

func remoteHas(t *testing.T, bare, branch string) string {
	t.Helper()
	out, err := git.Trusted().Run(ctx, bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return out
}

// pushFixture creates ticket "demo" with one service per repo (svc1, svc2, ...), the push
// mode of each given, starts it, commits one file per service in its checkout, and moves
// the ticket to the push phase.
func pushFixture(t *testing.T, modes []string, repos ...*testkit.Repo) (*fixture, *testkit.FakeGH, api.TicketStartResult) {
	t.Helper()
	f := newFixture(t, domain.SandboxContainer)
	gh := testkit.NewFakeGH()
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch), GH: gh,
		Global: config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, BranchPrefix: "rig/"}},
	})
	path := f.create("demo", repos...)
	yamlPath := filepath.Join(path, "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	doc := string(raw)
	for i, mode := range modes {
		name := "svc" + string(rune('1'+i))
		doc = strings.Replace(doc, "      branch: rig/demo/"+name, "      policy: {push: {mode: "+mode+"}}\n      branch: rig/demo/"+name, 1)
	}
	os.WriteFile(yamlPath, []byte(doc), 0o600)
	os.WriteFile(filepath.Join(path, "plan.md"), []byte("# Plan\n\nAdd the feature in two steps.\n"), 0o600)

	res := f.mustStart("demo", domain.SandboxContainer)
	for i, co := range res.Checkouts {
		commitFile(t, co.Path, "feature-"+string(rune('1'+i))+".txt", "work\n")
	}
	f.advanceTo("demo", domain.PhasePush)
	return f, gh, res
}

func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	testkit.RunGit(t, dir, "add", "-A")
	testkit.RunGit(t, dir, "-c", "user.name=agent", "-c", "user.email=a@b", "commit", "-qm", "add "+name)
	return testkit.RunGit(t, dir, "rev-parse", "HEAD")
}

func runPush(t *testing.T, f *fixture, service string) (api.PushRunResult, error) {
	t.Helper()
	return f.lc.Push(ctx, api.PushRunParams{ID: "demo", Service: service})
}

func TestPushIsRefusedOutsideThePushPhase(t *testing.T) {
	repo, bare := pushRepo(t)
	f := newFixture(t, domain.SandboxContainer)
	f.lc = lifecycle.New(lifecycle.Options{Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch),
		Global: config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, BranchPrefix: "rig/"}}})
	path := f.create("demo", repo)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), []byte(strings.Replace(string(raw), "      branch: rig/demo/svc1", "      policy: {push: {mode: push-branch}}\n      branch: rig/demo/svc1", 1)), 0o600)
	res := f.mustStart("demo", domain.SandboxContainer)
	commitFile(t, res.Checkouts[0].Path, "work.txt", "x\n")

	check := func(phase domain.Phase) {
		t.Helper()
		_, err := runPush(t, f, "")
		if apiCode(t, err) != api.CodeConflict || !strings.Contains(err.Error(), "phase "+string(phase)) || !strings.Contains(err.Error(), "review approval") {
			t.Errorf("in %s: error = %v", phase, err)
		}
	}
	check(domain.PhasePlan)
	if f.requires("demo", domain.GatePlanApproval) {
		f.mustApprove("demo", domain.GatePlanApproval, "")
	}
	for _, phase := range []domain.Phase{domain.PhaseImplement, domain.PhaseTest, domain.PhaseReview} {
		f.mustSet("demo", phase, "")
		check(phase)
	}
	if remoteHas(t, bare, "rig/demo/svc1") != "" {
		t.Error("something was pushed before the push phase")
	}
}

func TestMixedModesEachBehaveAsConfiguredAndTheTicketIsDone(t *testing.T) {
	r1, bare1 := pushRepo(t)
	r2, bare2 := pushRepo(t)
	f, gh, res := pushFixture(t, []string{"merge", "pr"}, r1, r2)
	tip1 := testkit.RunGit(t, res.Checkouts[0].Path, "rev-parse", "HEAD")
	tip2 := testkit.RunGit(t, res.Checkouts[1].Path, "rev-parse", "HEAD")

	got, err := runPush(t, f, "")
	if err != nil || !got.Done || len(got.Services) != 2 {
		t.Fatalf("Push = %+v, %v", got, err)
	}
	merged, pr := got.Services[0], got.Services[1]
	if merged.Mode != domain.PushMerge || !merged.Pushed || merged.Status != domain.PushDone || merged.Ref != "refs/heads/main" || merged.Error != "" {
		t.Errorf("svc1 = %+v", merged)
	}
	if remoteHas(t, bare1, "main") != tip1 || remoteHas(t, bare1, "rig/demo/svc1") != "" {
		t.Error("svc1: the base should be fast-forwarded and the feature branch not published")
	}
	if pr.Mode != domain.PushPR || !pr.Pushed || pr.PRURL == "" || pr.Status != domain.PushDone {
		t.Errorf("svc2 = %+v", pr)
	}
	if remoteHas(t, bare2, "rig/demo/svc2") != tip2 || remoteHas(t, bare2, "main") == tip2 {
		t.Error("svc2: the branch should be pushed and main left alone")
	}
	prs := gh.PRs()
	if len(prs) != 1 || prs[0].Title != "T demo (svc2)" || prs[0].Base != "main" || prs[0].Head != "rig/demo/svc2" || prs[0].Dir != r2.Dir {
		t.Fatalf("PRs = %+v", prs)
	}
	for _, want := range []string{"Ticket `demo`", "service `svc2`", "Add the feature in two steps.", "feature-2.txt"} {
		if !strings.Contains(prs[0].Body, want) {
			t.Errorf("the PR body lacks %q:\n%s", want, prs[0].Body)
		}
	}

	st := f.state("demo")
	if st.Phase != domain.PhaseDone || st.Services["svc1"].Push.Status != domain.PushDone || st.Services["svc2"].Push.PRURL != pr.PRURL {
		t.Errorf("state = phase %s, push %+v / %+v", st.Phase, st.Services["svc1"].Push, st.Services["svc2"].Push)
	}
	if again, err := runPush(t, f, ""); apiCode(t, err) != api.CodeConflict || !strings.Contains(err.Error(), "is done") {
		t.Errorf("a push after done = %+v, %v", again, err)
	}
}

func TestAPartialFailureDoesNotMarkDoneAndARerunRetriesOnlyTheFailedService(t *testing.T) {
	r1, bare1 := pushRepo(t)
	r2, bare2 := pushRepo(t)
	f, _, res := pushFixture(t, []string{"push-branch", "push-branch"}, r1, r2)

	// svc2's remote disappears
	moved := bare2 + ".away"
	os.Rename(bare2, moved)

	got, err := runPush(t, f, "")
	if err != nil || got.Done {
		t.Fatalf("Push = %+v, %v; a failure must not be an API error and must not finish the ticket", got, err)
	}
	if got.Services[0].Status != domain.PushDone || got.Services[0].Error != "" {
		t.Errorf("svc1 = %+v", got.Services[0])
	}
	if got.Services[1].Status != domain.PushFailed || got.Services[1].Error == "" || got.Services[1].Pushed {
		t.Errorf("svc2 = %+v", got.Services[1])
	}
	st := f.state("demo")
	if st.Phase != domain.PhasePush || st.Services["svc1"].Push.Status != domain.PushDone || st.Services["svc2"].Push.Status != domain.PushFailed || st.Services["svc2"].Push.Error == "" {
		t.Errorf("state = phase %s, %+v / %+v", st.Phase, st.Services["svc1"].Push, st.Services["svc2"].Push)
	}
	tip1 := remoteHas(t, bare1, "rig/demo/svc1")
	if tip1 == "" {
		t.Fatal("svc1 was not pushed")
	}

	// fix the remote; the rerun leaves svc1 alone (it would be rejected if it tried again, since we move its remote)
	os.Rename(moved, bare2)
	testkit.RunGit(t, bare1, "update-ref", "-d", "refs/heads/rig/demo/svc1") // delete svc1's pushed branch
	got, err = runPush(t, f, "")
	if err != nil || !got.Done {
		t.Fatalf("rerun = %+v, %v", got, err)
	}
	if !got.Services[0].Skipped || got.Services[0].Pushed || !strings.Contains(got.Services[0].Note, "earlier run") {
		t.Errorf("svc1 on the rerun = %+v; it was pushed already and must be skipped", got.Services[0])
	}
	if remoteHas(t, bare1, "rig/demo/svc1") != "" {
		t.Error("svc1 was pushed again by the rerun")
	}
	if got.Services[1].Status != domain.PushDone || !got.Services[1].Pushed || remoteHas(t, bare2, "rig/demo/svc2") == "" {
		t.Errorf("svc2 on the rerun = %+v", got.Services[1])
	}
	if f.state("demo").Phase != domain.PhaseDone {
		t.Error("the ticket should be done once both are pushed")
	}
	_ = res
}

func TestOneServiceCanBePushedOnItsOwn(t *testing.T) {
	r1, bare1 := pushRepo(t)
	r2, bare2 := pushRepo(t)
	f, _, _ := pushFixture(t, []string{"push-branch", "push-branch"}, r1, r2)
	got, err := runPush(t, f, "svc2")
	if err != nil || got.Done || len(got.Services) != 1 || got.Services[0].Service != "svc2" {
		t.Fatalf("Push = %+v, %v", got, err)
	}
	if remoteHas(t, bare2, "rig/demo/svc2") == "" || remoteHas(t, bare1, "rig/demo/svc1") != "" {
		t.Error("only svc2 should have been pushed")
	}
	if f.state("demo").Phase != domain.PhasePush {
		t.Error("the ticket is not done until every service is pushed")
	}
	if _, err := runPush(t, f, "nope"); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("an unknown service = %v", err)
	}
	if done, err := runPush(t, f, "svc1"); err != nil || !done.Done {
		t.Errorf("pushing the other one finishes the ticket: %+v, %v", done, err)
	}
}

func TestUncommittedWorkAndMissingSettingsAreFailuresNotSilentOmissions(t *testing.T) {
	repo, bare := pushRepo(t)
	f, _, res := pushFixture(t, []string{"push-branch"}, repo)
	os.WriteFile(filepath.Join(res.Checkouts[0].Path, "unsaved.txt"), []byte("wip\n"), 0o600)
	got, _ := runPush(t, f, "")
	if got.Services[0].Status != domain.PushFailed || !strings.Contains(got.Services[0].Error, "1 uncommitted change") || !strings.Contains(got.Services[0].Error, "rig review") {
		t.Errorf("result = %+v", got.Services[0])
	}
	if remoteHas(t, bare, "rig/demo/svc1") != "" {
		t.Error("a branch was pushed with uncommitted work left out silently")
	}
	os.Remove(filepath.Join(res.Checkouts[0].Path, "unsaved.txt"))
	if got, _ := runPush(t, f, ""); !got.Done {
		t.Errorf("after the work is gone: %+v", got)
	}

	// no push mode configured anywhere
	r2, _ := pushRepo(t)
	g, _, _ := pushFixture(t, nil, r2)
	if got, _ := runPush(t, g, ""); !strings.Contains(got.Services[0].Error, "no push mode") {
		t.Errorf("result = %+v", got.Services[0])
	}
}

func TestPushModeNoneReportsAndStillCompletesTheTicket(t *testing.T) {
	repo, bare := pushRepo(t)
	f, _, _ := pushFixture(t, []string{"none"}, repo)
	got, err := runPush(t, f, "")
	if err != nil || !got.Done || got.Services[0].Pushed || !strings.Contains(got.Services[0].Note, "nothing was pushed") || got.Services[0].Status != domain.PushDone {
		t.Errorf("Push = %+v, %v", got, err)
	}
	if remoteHas(t, bare, "rig/demo/svc1") != "" {
		t.Error("mode none pushed")
	}
}

func TestACloneCommitsReachTheRemoteOnlyThroughImportAndPush(t *testing.T) {
	repo, bare := pushRepo(t)
	f, _, res := pushFixture(t, []string{"push-branch"}, repo)
	tip := testkit.RunGit(t, res.Checkouts[0].Path, "rev-parse", "HEAD")
	if got, err := git.Trusted().Run(ctx, repo.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/rig/demo/svc1^{commit}"); err == nil && got == tip {
		t.Fatal("the commit was already in the main repository before the push")
	}
	if _, err := runPush(t, f, ""); err != nil {
		t.Fatal(err)
	}
	if remoteHas(t, bare, "rig/demo/svc1") != tip || testkit.RunGit(t, repo.Dir, "rev-parse", "rig/demo/svc1") != tip {
		t.Error("the push should have imported the branch into the main repository and then published it")
	}
}

func TestAPullRequestFailureIsReportedPerService(t *testing.T) {
	r1, bare1 := pushRepo(t)
	r2, bare2 := pushRepo(t)
	f, gh, _ := pushFixture(t, []string{"pr", "pr"}, r1, r2)
	gh.Unauthenticated = true
	got, err := runPush(t, f, "")
	if err != nil || got.Done {
		t.Fatalf("Push = %+v, %v", got, err)
	}
	for _, s := range got.Services {
		if s.Status != domain.PushFailed || !strings.Contains(s.Error, "gh auth login") {
			t.Errorf("%s = %+v", s.Service, s)
		}
	}
	if remoteHas(t, bare1, "rig/demo/svc1") != "" || remoteHas(t, bare2, "rig/demo/svc2") != "" {
		t.Error("branches were pushed although no pull request could be opened")
	}
	gh.Unauthenticated = false
	if got, _ := runPush(t, f, ""); !got.Done || len(gh.PRs()) != 2 {
		t.Errorf("after logging in: %+v, PRs %d", got, len(gh.PRs()))
	}
}

func TestTheMergeCommitPolicyIsHonoredPerService(t *testing.T) {
	repo, bare := pushRepo(t)
	f, _, _ := pushFixture(t, []string{"merge"}, repo)
	// main moves on at the remote
	other := filepath.Join(testkit.RealTempDir(t), "other")
	testkit.RunGit(t, filepath.Dir(other), "clone", "-q", bare, other)
	os.WriteFile(filepath.Join(other, "unrelated.txt"), []byte("u\n"), 0o644)
	testkit.RunGit(t, other, "add", "-A")
	testkit.RunGit(t, other, "-c", "user.name=o", "-c", "user.email=o@o", "commit", "-qm", "main moves on")
	testkit.RunGit(t, other, "push", "-q", "origin", "main")

	got, _ := runPush(t, f, "")
	if got.Services[0].Status != domain.PushFailed || !strings.Contains(got.Services[0].Error, "merge commit") {
		t.Fatalf("a diverged base without permission = %+v", got.Services[0])
	}

	path := filepath.Join(f.dir, "demo", "ticket.yaml")
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(raw), "policy: {push: {mode: merge}}", "policy: {push: {mode: merge, merge_commit: true}}", 1)), 0o600)
	got, _ = runPush(t, f, "")
	if !got.Done || !got.Services[0].Pushed {
		t.Errorf("with merge_commit: %+v", got.Services[0])
	}
}
