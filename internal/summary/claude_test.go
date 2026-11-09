package summary_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/summary"
)

const testKey = "sk-ant-test-0123456789"

type fakeAnthropic struct {
	mu      sync.Mutex
	srv     *httptest.Server
	headers []http.Header
	bodies  []string
	paths   []string
	status  int
	reply   string
}

func newFakeAnthropic(t *testing.T) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{status: 200, reply: `{"content":[{"type":"text","text":"- ships a header\n- tests pending"}]}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.bodies = append(f.bodies, string(raw))
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		status, reply := f.status, f.reply
		f.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnthropic) summarizer(t *testing.T, model string) summary.Summarizer {
	t.Helper()
	s, err := summary.NewClaude(f.srv.URL, model, summary.ProviderOptions{APIKey: func() (string, error) { return testKey, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClaudeSendsOnlyThePromptBuiltFromTheTicketsContent(t *testing.T) {
	f := newFakeAnthropic(t)
	in := input()
	text, err := f.summarizer(t, "claude-haiku-5-5").Summarize(context.Background(), in)
	if err != nil || text != "- ships a header\n- tests pending" {
		t.Fatalf("text %q, err %v", text, err)
	}
	if f.paths[0] != "POST /v1/messages" {
		t.Errorf("request = %s", f.paths[0])
	}
	h := f.headers[0]
	if h.Get("x-api-key") != testKey || h.Get("anthropic-version") != "2023-06-01" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", h)
	}

	// the body is exactly: model, max_tokens, one user message holding the prompt
	var body map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 3 || body["model"] != "claude-haiku-5-5" || body["max_tokens"] != float64(400) {
		t.Errorf("body fields = %v; nothing but model, max_tokens and messages may be sent", body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != summary.Prompt(in) {
		t.Errorf("the message is not exactly the ticket's prompt: %v", m)
	}
	if strings.Contains(f.bodies[0], testKey) {
		t.Error("the API key is in the request body")
	}
}

func TestContentFromOutsideTheTicketNeverReachesTheRequest(t *testing.T) {
	f := newFakeAnthropic(t)
	in := input() // only the ticket's plan, tasks and diffs
	if _, err := f.summarizer(t, "").Summarize(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"/etc/passwd", "ANTHROPIC_API_KEY", "TOP-SECRET", testKey} {
		if strings.Contains(f.bodies[0], leak) {
			t.Errorf("the request mentions %q", leak)
		}
	}
	for _, want := range []string{"Use a locale header.", "added line"} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("the request lacks the ticket's %q", want)
		}
	}
}

func TestClaudeDefaultsToASmallModel(t *testing.T) {
	f := newFakeAnthropic(t)
	s := f.summarizer(t, "")
	if s.Model() != summary.DefaultClaudeModel || s.Name() != "claude" || !s.Remote() {
		t.Errorf("%s %s remote=%v", s.Name(), s.Model(), s.Remote())
	}
	s.Summarize(context.Background(), input())
	if !strings.Contains(f.bodies[0], `"model":"claude-haiku-5-5"`) {
		t.Errorf("body = %s", f.bodies[0])
	}
}

func TestClaudeErrorsAreExplainedWithoutLeakingTheKey(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a bad key", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "refused the key (401); check ANTHROPIC_API_KEY: invalid x-api-key"},
		{"forbidden", 403, `{}`, "refused the key (403)"},
		{"rate limited", 429, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, "rate limiting this key (429): slow down"},
		{"overloaded", 529, `{"error":{"type":"overloaded_error","message":"Overloaded"}}`, "answered 529: Overloaded"},
		{"a bare 500", 500, `oops`, "answered 500"},
		{"no text", 200, `{"content":[]}`, "empty summary"},
		{"only a tool block", 200, `{"content":[{"type":"tool_use"}]}`, "empty summary"},
	}
	for _, tt := range tests {
		f := newFakeAnthropic(t)
		f.status, f.reply = tt.status, tt.body
		_, err := f.summarizer(t, "").Summarize(context.Background(), input())
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), testKey) {
			t.Errorf("%s: the key is in the error: %v", tt.name, err)
		}
	}
}

func TestClaudeNeedsAKeyAndSaysWhenItIsMissing(t *testing.T) {
	f := newFakeAnthropic(t)
	missing, _ := summary.NewClaude(f.srv.URL, "", summary.ProviderOptions{APIKey: func() (string, error) { return "", errors.New("secret ANTHROPIC_API_KEY not found") }})
	if _, err := missing.Summarize(context.Background(), input()); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY not found") {
		t.Errorf("err = %v", err)
	}
	empty, _ := summary.NewClaude(f.srv.URL, "", summary.ProviderOptions{APIKey: func() (string, error) { return "  ", nil }})
	if _, err := empty.Summarize(context.Background(), input()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v", err)
	}
	if len(f.bodies) != 0 {
		t.Error("a request was sent without a key")
	}
}

func TestTheKeyIsLookedUpAtEveryRequest(t *testing.T) {
	f := newFakeAnthropic(t)
	key := "first"
	s, _ := summary.NewClaude(f.srv.URL, "", summary.ProviderOptions{APIKey: func() (string, error) { return key, nil }})
	s.Summarize(context.Background(), input())
	key = "second"
	s.Summarize(context.Background(), input())
	if f.headers[0].Get("x-api-key") != "first" || f.headers[1].Get("x-api-key") != "second" {
		t.Errorf("keys sent: %q %q", f.headers[0].Get("x-api-key"), f.headers[1].Get("x-api-key"))
	}
}

func TestClaudeEndpointsMustBeSecureExceptOnThisMachine(t *testing.T) {
	key := summary.ProviderOptions{APIKey: func() (string, error) { return "k", nil }}
	for endpoint, ok := range map[string]bool{
		"":                                     true,
		"https://api.anthropic.com":            true,
		"https://proxy.corp.example/anthropic": true,
		"http://127.0.0.1:8080":                true,
		"http://localhost:9":                   true,
		"http://api.anthropic.com":             false,
		"http://gpu-box.lan":                   false,
		"ftp://x":                              false,
		"not a url":                            false,
	} {
		_, err := summary.NewClaude(endpoint, "", key)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", endpoint, err, ok)
		}
	}
}

func TestTheKeyIsScrubbedFromTransportErrors(t *testing.T) {
	leaky := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed while sending header x-api-key: " + r.Header.Get("x-api-key"))
	})
	s, _ := summary.NewClaude("https://api.anthropic.com", "", summary.ProviderOptions{HTTP: leaky, APIKey: func() (string, error) { return testKey, nil }})
	_, err := s.Summarize(context.Background(), input())
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "[key]") {
		t.Errorf("err = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
