package summary_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/summary"
)

func input() summary.Input {
	return summary.Input{
		Ticket: "demo", Title: "Add locale",
		Plan: "Use a locale header.", Tasks: "- [x] header\n- [ ] tests",
		Diffs: []summary.ServiceDiff{
			{Service: "api", Stat: summary.Stats{Files: 2, Added: 30, Deleted: 4}, Files: []string{"a.go", "b.go"}, Patch: "+added line\n-removed line\n"},
		},
	}
}

func TestPromptCarriesTheTicketsOwnContent(t *testing.T) {
	p := summary.Prompt(input())
	for _, want := range []string{"Ticket demo: Add locale", "## Plan\nUse a locale header.", "## Tasks\n- [x] header", "service api: 2 files, +30 -4", "a.go\nb.go", "```diff\n+added line", "at most five short bullet points"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestLargeDiffsAreTruncatedAndShareTheBudgetFairly(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	in := summary.Input{Ticket: "t", Diffs: []summary.ServiceDiff{
		{Service: "one", Patch: big}, {Service: "two", Patch: "small patch\n"}, {Service: "three", Patch: big},
	}}
	p := summary.Prompt(in)
	if len(p) > summary.MaxPatchBytes+2000 {
		t.Errorf("prompt is %d bytes, over the budget", len(p))
	}
	if !strings.Contains(p, "small patch") {
		t.Error("a small diff was cut because a large one came first")
	}
	if strings.Count(p, "[diff truncated]") != 2 {
		t.Errorf("both large diffs should be marked truncated:\n%d bytes", len(p))
	}
}

func TestHashIdentifiesContent(t *testing.T) {
	a, b := input(), input()
	if a.Hash() != b.Hash() {
		t.Error("equal inputs hash differently")
	}
	b.Plan += " More."
	if a.Hash() == b.Hash() {
		t.Error("a changed plan kept the hash")
	}
	c := input()
	c.Diffs[0].Patch += "+one more\n"
	if a.Hash() == c.Hash() {
		t.Error("a changed diff kept the hash")
	}
}

func TestEmptyInputsAreNotWorthSummarizing(t *testing.T) {
	tests := []struct {
		name  string
		in    summary.Input
		empty bool
	}{
		{"nothing", summary.Input{Ticket: "t", Title: "T"}, true},
		{"blank files", summary.Input{Plan: "  \n", Tasks: "\n"}, true},
		{"a plan", summary.Input{Plan: "x"}, false},
		{"tasks", summary.Input{Tasks: "- [ ] x"}, false},
		{"a diff with files", summary.Input{Diffs: []summary.ServiceDiff{{Stat: summary.Stats{Files: 1}}}}, false},
		{"a diff with a patch", summary.Input{Diffs: []summary.ServiceDiff{{Patch: "+x"}}}, false},
		{"a diff of nothing", summary.Input{Diffs: []summary.ServiceDiff{{Service: "api"}}}, true},
	}
	for _, tt := range tests {
		if tt.in.Empty() != tt.empty {
			t.Errorf("%s: Empty = %v", tt.name, !tt.empty)
		}
	}
}

func TestNoneSaysNothingAndSendsNothing(t *testing.T) {
	s, err := summary.New(config.Summarizer{Provider: "none"}, summary.ProviderOptions{})
	if err != nil || s.Name() != "none" || s.Remote() || s.Model() != "" {
		t.Fatalf("none = %v, %v", s, err)
	}
	if text, err := s.Summarize(context.Background(), input()); text != "" || err != nil {
		t.Errorf("none summarized: %q, %v", text, err)
	}
	if s2, _ := summary.New(config.Summarizer{}, summary.ProviderOptions{}); s2.Name() != "none" {
		t.Error("an empty provider is not none")
	}
}

func TestNewRejectsWhatItCannotBuild(t *testing.T) {
	for name, c := range map[string]config.Summarizer{
		"ollama without a model":      {Provider: "ollama"},
		"a bad endpoint":              {Provider: "ollama", Model: "m", Endpoint: "not a url"},
		"a file endpoint":             {Provider: "ollama", Model: "m", Endpoint: "file:///etc/passwd"},
		"an unknown provider":         {Provider: "magic"},
		"claude without a key source": {Provider: "claude"},
	} {
		if _, err := summary.New(c, summary.ProviderOptions{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// fakeOllama records requests and answers with whatever the test sets.
type fakeOllama struct {
	mu     sync.Mutex
	srv    *httptest.Server
	reqs   []map[string]any
	paths  []string
	status int
	body   string
	delay  time.Duration
	hits   atomic.Int32
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{status: 200, body: `{"response":"- does the thing\n- half done"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		status, body, delay := f.status, f.body, f.delay
		f.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOllama) set(status int, body string) {
	f.mu.Lock()
	f.status, f.body = status, body
	f.mu.Unlock()
}

func (f *fakeOllama) summarizer(t *testing.T) summary.Summarizer {
	t.Helper()
	s, err := summary.NewOllama(f.srv.URL, "llama3:8b", summary.ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOllamaSendsTheRightRequestAndReturnsTheText(t *testing.T) {
	f := newFakeOllama(t)
	text, err := f.summarizer(t).Summarize(context.Background(), input())
	if err != nil || text != "- does the thing\n- half done" {
		t.Fatalf("text %q, err %v", text, err)
	}
	if f.paths[0] != "POST /api/generate" {
		t.Errorf("request = %s", f.paths[0])
	}
	req := f.reqs[0]
	if req["model"] != "llama3:8b" || req["stream"] != false || !strings.Contains(req["prompt"].(string), "Use a locale header.") {
		t.Errorf("body = %v", req)
	}
}

func TestOllamaErrorsAreExplained(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"missing model", 404, `{"error":"model 'llama3:8b' not found, try pulling it first"}`, "ollama pull llama3:8b"},
		{"a server error", 500, `{"error":"out of memory"}`, "answered 500: out of memory"},
		{"a bare status", 502, `<html>bad gateway</html>`, "answered 502"},
		{"an error in a 200", 200, `{"error":"model crashed"}`, "model crashed"},
		{"an empty answer", 200, `{"response":"  \n"}`, "empty summary"},
		{"not json", 200, `plain text`, "empty summary"},
	}
	for _, tt := range tests {
		f := newFakeOllama(t)
		f.set(tt.status, tt.body)
		_, err := f.summarizer(t).Summarize(context.Background(), input())
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
}

func TestOllamaUnreachableIsAnError(t *testing.T) {
	f := newFakeOllama(t)
	s := f.summarizer(t)
	f.srv.Close()
	if _, err := s.Summarize(context.Background(), input()); err == nil || !strings.Contains(err.Error(), "ollama at") {
		t.Errorf("err = %v", err)
	}
}

func TestOllamaHonoursCancellationAndItsTimeout(t *testing.T) {
	f := newFakeOllama(t)
	f.delay = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := f.summarizer(t).Summarize(ctx, input()); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
	s, _ := summary.NewOllama(f.srv.URL, "m", summary.ProviderOptions{Timeout: 50 * time.Millisecond})
	start = time.Now()
	if _, err := s.Summarize(context.Background(), input()); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("the client timeout did not apply: %v after %v", err, time.Since(start))
	}
}

func TestOllamaRemoteMeansNotThisMachine(t *testing.T) {
	for endpoint, remote := range map[string]bool{
		"": false, "http://localhost:11434": false, "http://127.0.0.1:1": false, "http://[::1]:11434": false,
		"http://gpu-box.lan:11434": true, "https://ollama.example.com": true,
	} {
		s, err := summary.NewOllama(endpoint, "m", summary.ProviderOptions{})
		if err != nil || s.Remote() != remote {
			t.Errorf("%q: remote = %v, err %v; want %v", endpoint, s != nil && s.Remote(), err, remote)
		}
	}
}

func TestInfoWireShape(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(api.SummaryInfo{Text: "t", Backend: "ollama", Model: "m", At: &at, Files: 2, Added: 3, Deleted: 1})
	if string(raw) != `{"text":"t","backend":"ollama","model":"m","remote":false,"at":"2026-11-09T12:00:00Z","files":2,"added":3,"deleted":1}` {
		t.Errorf("json = %s", raw)
	}
}
