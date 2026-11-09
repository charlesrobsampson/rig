package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
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
