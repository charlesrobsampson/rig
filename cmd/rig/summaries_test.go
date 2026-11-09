package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/summary"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestSummariesAreWrittenUnderTheTableWithTheirLabels(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	tickets := []api.TicketSummary{
		{ID: "alpha", AISummary: &api.SummaryInfo{Text: "- adds a header\n- tests missing", Backend: "ollama", Model: "llama3:8b", Files: 3, Added: 45, Deleted: 12, At: &at}},
		{ID: "beta"},
		{ID: "gamma", AISummary: &api.SummaryInfo{Text: "- old note", Backend: "claude", Remote: true, Error: "ollama answered 500"}},
		{ID: "delta", AISummary: &api.SummaryInfo{Backend: "ollama", Error: "no model"}},
	}
	var out bytes.Buffer
	writeSummaries(&out, tickets)
	got := out.String()
	for _, want := range []string{
		"alpha  summary (advisory; ollama llama3:8b; 3 files +45 -12, ", "    - adds a header", "    - tests missing",
		"gamma  summary (advisory; claude, REMOTE: the ticket's content was sent to it; 0 files +0 -0)",
		"(the summary above is out of date; refresh failed: ollama answered 500)",
		"delta  summary", "(refresh failed: no model)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "beta") {
		t.Errorf("a ticket without a summary was listed:\n%s", got)
	}

	out.Reset()
	writeSummaries(&out, []api.TicketSummary{{ID: "beta"}})
	if !strings.Contains(out.String(), "no summaries (set summarizer.provider") {
		t.Errorf("no-summaries message: %q", out.String())
	}
}

func TestListSummariesFlagPrintsThemAndPlainListDoesNot(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	plain, _, _ := rig(t, d, "list")
	withFlag, _, code := rig(t, d, "list", "--summaries")
	if code != 0 || strings.Contains(plain, "summary") || !strings.HasPrefix(withFlag, plain) || !strings.Contains(withFlag, "no summaries") {
		t.Errorf("code %d\nplain:\n%s\nwith the flag:\n%s", code, plain, withFlag)
	}
	if out, _, _ := rig(t, d, "list", "--json"); strings.Contains(out, "ai_summary") {
		t.Errorf("a ticket without a summary has an ai_summary field: %s", out)
	}
}

func TestTheDaemonGathersOnlyTheTicketsOwnContent(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	ticketDir := filepath.Join(home, "tickets", "demo")
	os.WriteFile(filepath.Join(ticketDir, "plan.md"), []byte("# Plan\nadd a header\n"), 0o600)
	os.WriteFile(filepath.Join(ticketDir, "tasks.md"), []byte("- [x] header\n"), 0o600)
	os.WriteFile(filepath.Join(ticketDir, "secret.txt"), []byte("TOP-SECRET"), 0o600) // not a lifecycle file
	work := filepath.Join(ticketDir, "svc1")
	os.WriteFile(filepath.Join(work, "feature.txt"), []byte("hello\n"), 0o600)
	testkit.RunGit(t, work, "add", "-A")
	testkit.RunGit(t, work, "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-qm", "work")

	states := state.NewManager(clock.Real())
	reg := registry.New(filepath.Join(home, "tickets"), states)
	lc := lifecycle.New(lifecycle.Options{Dir: filepath.Join(home, "tickets"), States: states, Global: config.Global{}})
	in, err := summaryInputs{lc: lc, reg: reg}.Gather(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if in.Title != "Ticket demo" || !strings.Contains(in.Plan, "add a header") || !strings.Contains(in.Tasks, "header") {
		t.Errorf("input = %+v", in)
	}
	if len(in.Diffs) != 1 || in.Diffs[0].Service != "svc1" || in.Diffs[0].Stat.Files != 1 || in.Diffs[0].Stat.Added != 1 || !strings.Contains(in.Diffs[0].Patch, "+hello") {
		t.Errorf("diffs = %+v", in.Diffs)
	}
	if prompt := summary.Prompt(in); strings.Contains(prompt, "TOP-SECRET") {
		t.Error("a file outside the ticket's lifecycle files reached the prompt")
	}
	if _, err := (summaryInputs{lc: lc, reg: reg}).Gather(context.Background(), "ghost"); err == nil {
		t.Error("an unknown ticket was gathered")
	}
}

func TestADaemonWithoutASummarizerRunsNoJob(t *testing.T) {
	g := config.Global{}
	s := newSummaryService(g, nil, nil, clock.Real(), slogDiscard())
	if s.Enabled() {
		t.Error("summaries are on with no provider")
	}
	g.Summarizer = config.Summarizer{Provider: "ollama"} // no model: cannot be built
	if s := newSummaryService(g, nil, nil, clock.Real(), slogDiscard()); s.Enabled() {
		t.Error("a misconfigured summarizer turned summaries on")
	}
	g.Summarizer = config.Summarizer{Provider: "ollama", Model: "m"}
	g.Paths.Home = t.TempDir()
	if s := newSummaryService(g, nil, nil, clock.Real(), slogDiscard()); !s.Enabled() {
		t.Error("a valid ollama config did not enable summaries")
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTheClaudeSummarizerFindsItsKeyThroughTheSecretsResolver(t *testing.T) {
	g := config.Global{Summarizer: config.Summarizer{Provider: "claude"}}
	g.Paths.Home = t.TempDir()
	var asked [][]string
	secret := func(names []string) (map[string]string, error) {
		asked = append(asked, names)
		return map[string]string{"ANTHROPIC_API_KEY": "sk-test"}, nil
	}
	if s := newSummaryService(g, secret, nil, clock.Real(), slogDiscard()); !s.Enabled() {
		t.Fatal("claude with a resolver did not enable summaries")
	}
	key, err := anthropicKey(secret)()
	if err != nil || key != "sk-test" || len(asked) != 1 || asked[0][0] != "ANTHROPIC_API_KEY" {
		t.Errorf("key %q, err %v, asked %v", key, err, asked)
	}
	// without a resolver the provider cannot be built, and the daemon carries on without summaries
	if s := newSummaryService(g, nil, nil, clock.Real(), slogDiscard()); s.Enabled() {
		t.Error("claude without a resolver enabled summaries")
	}
}

// jiraEnv is a daemon whose config points at a fake Jira, with the credential in the
// (fake) Keychain.
func jiraEnv(t *testing.T, srvURL string, withToken bool) (home string, d deps, repo *testkit.Repo) {
	t.Helper()
	home = rigEnv(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"jira":{"base_url":"`+srvURL+`","email":"me@acme.com"}}`), 0o600)
	t.Setenv("RIG_CONFIG", cfg)
	keychain := func(_ context.Context, args ...string) (string, string, int, error) {
		if withToken && strings.Contains(strings.Join(args, " "), "JIRA_API_TOKEN") {
			return "jira-token-xyz\n", "", 0, nil
		}
		return "", "item could not be found", 44, nil
	}
	daemon := &inProcessDaemon{backend: testkit.NewFakeTerminal(), docker: testkit.NewFakeDocker(), keychain: keychain, gh: testkit.NewFakeGH()}
	t.Cleanup(func() { daemon.shutdown(t) })
	return home, deps{spawner: daemon.spawn, backend: daemon.backend, docker: daemon.docker, keychain: keychain, gh: daemon.gh}, testkit.NewRepo(t)
}

func TestNewFromJiraSeedsThePlanAndKeepsTheCredentialOutOfEverything(t *testing.T) {
	var gotUser, gotPass string
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		gotUser, gotPass, _ = r.BasicAuth()
		io.WriteString(w, `{"key":"PROJ-412","fields":{"summary":"Dashboard for users","description":"h2. Goal\nShow *bold* things.\n* one"}}`)
	}))
	defer srv.Close()
	home, d, repo := jiraEnv(t, srv.URL, true)

	stdout, stderr, code := rig(t, d, "new", "--from", "jira:PROJ-412", "--service", "api="+repo.Dir)
	if code != 0 {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if gotUser != "me@acme.com" || gotPass != "jira-token-xyz" || len(methods) != 1 || methods[0] != "GET /rest/api/2/issue/PROJ-412" {
		t.Errorf("Jira saw user %q pass %q requests %v", gotUser, gotPass, methods)
	}
	dir := filepath.Join(home, "tickets", "proj-412")
	plan, _ := os.ReadFile(filepath.Join(dir, "plan.md"))
	yaml, _ := os.ReadFile(filepath.Join(dir, "ticket.yaml"))
	for _, want := range []string{"Dashboard for users", "## Goal", "Show **bold** things.", "- one", srv.URL + "/browse/PROJ-412"} {
		if !strings.Contains(string(plan), want) {
			t.Errorf("plan.md lacks %q:\n%s", want, plan)
		}
	}
	if !strings.Contains(string(yaml), "jira") || !strings.Contains(string(yaml), "PROJ-412") {
		t.Errorf("ticket.yaml does not record the source:\n%s", yaml)
	}

	// the credential is nowhere it should not be
	log, _ := os.ReadFile(filepath.Join(home, "rigd.log"))
	var files []string
	filepath.Walk(home, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && !strings.HasSuffix(p, ".sock") {
			files = append(files, p)
		}
		return nil
	})
	for _, f := range files {
		if raw, _ := os.ReadFile(f); strings.Contains(string(raw), "jira-token-xyz") {
			t.Errorf("the credential was written to %s", f)
		}
	}
	if strings.Contains(string(log)+stdout+stderr, "jira-token-xyz") {
		t.Error("the credential is in a log or output")
	}
}

func TestNewFromJiraErrorsMapToExitCodesAndSayWhatToDo(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()
	_, d, repo := jiraEnv(t, srv.URL, true)
	tests := []struct {
		name   string
		status int
		args   []string
		code   int
		want   string
	}{
		{"an unknown issue", 404, []string{"new", "--from", "jira:PROJ-9", "--service", "api=" + repo.Dir}, 5, "PROJ-9 does not exist"},
		{"a bad credential", 401, []string{"new", "--from", "jira:PROJ-9", "--service", "api=" + repo.Dir}, 4, "rejected the credentials"},
		{"a malformed key", 200, []string{"new", "--from", "jira:nonsense", "--service", "api=" + repo.Dir}, 2, "not a Jira issue key"},
	}
	for _, tt := range tests {
		status = tt.status
		_, stderr, code := rig(t, d, tt.args...)
		if code != tt.code || !strings.Contains(stderr, tt.want) {
			t.Errorf("%s: code %d (want %d), stderr %q", tt.name, code, tt.code, stderr)
		}
	}
}

func TestNewFromJiraWithoutACredentialOrConfigExplainsWhatIsMissing(t *testing.T) {
	_, d, repo := jiraEnv(t, "https://acme.atlassian.net", false)
	_, stderr, code := rig(t, d, "new", "--from", "jira:PROJ-9", "--service", "api="+repo.Dir)
	if code == 0 || !strings.Contains(stderr, "JIRA_API_TOKEN") {
		t.Errorf("code %d, stderr %q; it should name the missing secret", code, stderr)
	}

	// no base_url at all
	rigEnv(t)
	plain := &inProcessDaemon{backend: testkit.NewFakeTerminal(), docker: testkit.NewFakeDocker(), gh: testkit.NewFakeGH()}
	t.Cleanup(func() { plain.shutdown(t) })
	d2 := deps{spawner: plain.spawn, backend: plain.backend, docker: plain.docker, gh: plain.gh}
	_, stderr, code = rig(t, d2, "new", "--from", "jira:PROJ-9", "--service", "api="+repo.Dir)
	if code != 2 || !strings.Contains(stderr, "jira.base_url") {
		t.Errorf("unconfigured: code %d, stderr %q", code, stderr)
	}
}

// githubEnv is a daemon whose config points at a fake GitHub API.
func githubEnv(t *testing.T, apiURL string, token string) (home string, d deps, repo *testkit.Repo) {
	t.Helper()
	home = rigEnv(t)
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"github":{"api_url":"`+apiURL+`"}}`), 0o600)
	t.Setenv("RIG_CONFIG", cfg)
	keychain := func(_ context.Context, args ...string) (string, string, int, error) {
		if token != "" && strings.Contains(strings.Join(args, " "), "GITHUB_TOKEN") {
			return token + "\n", "", 0, nil
		}
		return "", "item could not be found", 44, nil
	}
	daemon := &inProcessDaemon{backend: testkit.NewFakeTerminal(), docker: testkit.NewFakeDocker(), keychain: keychain, gh: testkit.NewFakeGH()}
	t.Cleanup(func() { daemon.shutdown(t) })
	return home, deps{spawner: daemon.spawn, backend: daemon.backend, docker: daemon.docker, keychain: keychain, gh: daemon.gh}, testkit.NewRepo(t)
}

func TestNewFromGitHubSeedsThePlanWithAndWithoutAToken(t *testing.T) {
	var auths, paths []string
	status, body := 200, `{"number":123,"title":"Add a locale header","body":"## Why\nBecause.","html_url":"https://github.com/acme/api/issues/123"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	home, d, repo := githubEnv(t, srv.URL, "ghp_clitoken")
	_, stderr, code := rig(t, d, "new", "--from", "github:acme/api#123", "--service", "api="+repo.Dir)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if auths[0] != "Bearer ghp_clitoken" || paths[0] != "GET /repos/acme/api/issues/123" {
		t.Errorf("GitHub saw %q %q", auths[0], paths[0])
	}
	dir := filepath.Join(home, "tickets", "acme-api-123")
	plan, _ := os.ReadFile(filepath.Join(dir, "plan.md"))
	yaml, _ := os.ReadFile(filepath.Join(dir, "ticket.yaml"))
	for _, want := range []string{"Add a locale header", "## Why", "Because.", "https://github.com/acme/api/issues/123"} {
		if !strings.Contains(string(plan), want) {
			t.Errorf("plan.md lacks %q:\n%s", want, plan)
		}
	}
	if !strings.Contains(string(yaml), "github") || !strings.Contains(string(yaml), "acme/api#123") {
		t.Errorf("ticket.yaml does not record the source:\n%s", yaml)
	}
	log, _ := os.ReadFile(filepath.Join(home, "rigd.log"))
	if strings.Contains(string(log), "ghp_clitoken") {
		t.Error("the token is in the daemon log")
	}
}

func TestNewFromGitHubWorksAnonymouslyForPublicRepositories(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{"number":5,"title":"Public issue","body":null}`)
	}))
	defer srv.Close()
	home, d, repo := githubEnv(t, srv.URL, "") // no token anywhere
	if _, stderr, code := rig(t, d, "new", "--from", "github:acme/api#5", "--service", "api="+repo.Dir); code != 0 || auth != "" {
		t.Fatalf("code %d, auth %q, stderr %q", code, auth, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "tickets", "acme-api-5", "plan.md")); err != nil {
		t.Error(err)
	}
}

func TestNewFromGitHubErrorsMapToExitCodes(t *testing.T) {
	status, body := 404, `{"message":"Not Found"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	_, d, repo := githubEnv(t, srv.URL, "")
	run := func(name string, st int, b, ref string, code int, want string) {
		t.Helper()
		status, body = st, b
		_, stderr, got := rig(t, d, "new", "--from", ref, "--service", "api="+repo.Dir)
		if got != code || !strings.Contains(stderr, want) {
			t.Errorf("%s: code %d (want %d), stderr %q", name, got, code, stderr)
		}
	}
	run("an unknown issue", 404, `{}`, "github:acme/api#9", 5, "does not exist")
	run("a pull request", 200, `{"title":"T","pull_request":{"url":"x"}}`, "github:acme/api#9", 2, "is a pull request, not an issue")
	run("a bad token", 401, `{}`, "github:acme/api#9", 4, "rejected the credentials")
	run("a malformed reference", 200, `{}`, "github:nonsense", 2, "not a GitHub issue reference")
}
