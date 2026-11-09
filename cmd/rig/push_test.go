package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func repoWithRemote(t *testing.T) (*testkit.Repo, string) {
	t.Helper()
	repo := testkit.NewRepo(t)
	bare := filepath.Join(testkit.RealTempDir(t), "remote.git")
	testkit.RunGit(t, filepath.Dir(bare), "init", "-q", "--bare", "-b", "main", bare)
	repo.Git("remote", "add", "origin", bare)
	repo.Git("push", "-q", "origin", "main")
	return repo, bare
}

// pushReady creates ticket "demo" with one service per repo and the given push modes,
// starts it on the host, commits a file in each checkout and walks the ticket to the
// push phase through the CLI.
func pushReady(t *testing.T, d deps, home string, modes []string, repos ...*testkit.Repo) {
	t.Helper()
	newTicket(t, d, "demo", repos...)
	agreeContract(t, home, "demo")
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	doc := string(raw)
	for i, mode := range modes {
		name := "svc" + string(rune('1'+i))
		doc = strings.Replace(doc, "      branch: rig/demo/"+name, "      policy: {push: {mode: "+mode+"}}\n      branch: rig/demo/"+name, 1)
	}
	os.WriteFile(yamlPath, []byte(doc), 0o600)
	if _, stderr, code := rig(t, d, "start", "demo", "--sandbox", "host"); code != 0 {
		t.Fatalf("start: %q", stderr)
	}
	for i := range repos {
		dir := filepath.Join(home, "tickets", "demo", "svc"+string(rune('1'+i)))
		os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("work\n"), 0o600)
		testkit.RunGit(t, dir, "add", "-A")
		testkit.RunGit(t, dir, "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-qm", "work")
	}
	for _, args := range [][]string{{"approve", "demo", "plan_approval"}, {"phase", "demo", "implement"}, {"phase", "demo", "test"}, {"phase", "demo", "review"}, {"approve", "demo", "review_approval"}, {"phase", "demo", "push"}} {
		if _, stderr, code := rig(t, d, args...); code != 0 {
			t.Fatalf("%v: %q", args, stderr)
		}
	}
}

func TestPushReportsEachServiceAndFinishesTheTicket(t *testing.T) {
	home, d, _ := newEnv(t)
	r1, bare1 := repoWithRemote(t)
	r2, bare2 := repoWithRemote(t)
	pushReady(t, d, home, []string{"push-branch", "pr"}, r1, r2)

	stdout, stderr, code := rig(t, d, "push", "demo")
	if code != 0 {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{"SERVICE", "svc1", "push-branch", "pushed", "refs/heads/rig/demo/svc1", "svc2", "pr", "https://github.com/acme/", "the ticket is done"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	for bare, branch := range map[string]string{bare1: "rig/demo/svc1", bare2: "rig/demo/svc2"} {
		if _, err := git.Trusted().Run(ctx, bare, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
			t.Errorf("%s was not pushed: %v", branch, err)
		}
	}
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "done") {
		t.Errorf("list:\n%s", listing)
	}
	if _, stderr, code := rig(t, d, "push", "demo"); code != 1 || !strings.Contains(stderr, "is done") {
		t.Errorf("a second push: code %d, stderr %q", code, stderr)
	}
}

func TestPushJSONHasTheDocumentedShape(t *testing.T) {
	home, d, _ := newEnv(t)
	r1, _ := repoWithRemote(t)
	pushReady(t, d, home, []string{"none"}, r1)
	stdout, _, code := rig(t, d, "push", "demo", "--json")
	var res api.PushRunResult
	if err := json.Unmarshal([]byte(stdout), &res); code != 0 || err != nil {
		t.Fatalf("code %d, %v, %q", code, err, stdout)
	}
	if !res.Done || len(res.Services) != 1 || res.Services[0].Mode != domain.PushNone || res.Services[0].Status != domain.PushDone || res.Services[0].Pushed || res.Summary.ID != "demo" {
		t.Errorf("result = %+v", res)
	}
	var raw map[string]any
	json.Unmarshal([]byte(stdout), &raw)
	svc := raw["services"].([]any)[0].(map[string]any)
	for _, key := range []string{"service", "mode", "status", "pushed"} {
		if _, ok := svc[key]; !ok {
			t.Errorf("the JSON lacks %q: %v", key, svc)
		}
	}
}

func TestPushFailureExitsNonZeroAndASecondRunRetriesOnlyThatService(t *testing.T) {
	home, d, _ := newEnv(t)
	r1, _ := repoWithRemote(t)
	r2, bare2 := repoWithRemote(t)
	pushReady(t, d, home, []string{"push-branch", "push-branch"}, r1, r2)
	away := bare2 + ".away"
	os.Rename(bare2, away)

	stdout, stderr, code := rig(t, d, "push", "demo")
	if code != 1 || !strings.Contains(stdout, "failed") || !strings.Contains(stderr, "1 service(s) were not pushed") || !strings.Contains(stderr, "rig push demo again") {
		t.Errorf("code %d, stdout:\n%s\nstderr %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "the ticket is done") {
		t.Error("a partial failure must not finish the ticket")
	}

	os.Rename(away, bare2)
	stdout, _, code = rig(t, d, "push", "demo")
	if code != 0 || !strings.Contains(stdout, "already pushed") || !strings.Contains(stdout, "the ticket is done") {
		t.Errorf("rerun: code %d, stdout:\n%s", code, stdout)
	}
}

func TestPushErrors(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	_ = home
	for _, tt := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"push", "demo"}, 1, "phase plan"},
		{[]string{"push", "ghost"}, 5, "ghost"},
		{[]string{"push", "demo", "nope"}, 1, ""},
		{[]string{"push"}, 2, ""},
	} {
		_, stderr, code := rig(t, d, tt.args...)
		if code != tt.code && !(tt.args[len(tt.args)-1] == "nope" && code == 1) || !strings.Contains(stderr, tt.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q", tt.args, code, tt.code, stderr)
		}
	}
}
