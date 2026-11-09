package ticket_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/ticket"
)

const jiraToken = "ATATT3xFfGF0-secret-token"

type fakeJira struct {
	mu      sync.Mutex
	srv     *httptest.Server
	reqs    []*http.Request
	status  int
	body    string
	headers http.Header
}

func newFakeJira(t *testing.T) *fakeJira {
	t.Helper()
	f := &fakeJira{status: 200, body: `{"key":"PROJ-412","fields":{"summary":"  Dashboard for users ","description":"h2. Goal\nShow *bold* things and {{code}}.\n* one\n** two"}}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJira) source(email string) ticket.Jira {
	return ticket.Jira{BaseURL: f.srv.URL, Email: email, Token: func() (string, error) { return jiraToken, nil }}
}

func TestJiraSeedsTitleDescriptionAndURL(t *testing.T) {
	f := newFakeJira(t)
	seed, err := f.source("me@acme.com").Fetch(context.Background(), "proj-412")
	if err != nil {
		t.Fatal(err)
	}
	if seed.ID != "PROJ-412" || seed.Title != "Dashboard for users" || seed.URL != f.srv.URL+"/browse/PROJ-412" {
		t.Errorf("seed = %+v", seed)
	}
	if want := "## Goal\nShow **bold** things and `code`.\n- one\n  - two"; seed.Description != want {
		t.Errorf("description = %q, want %q", seed.Description, want)
	}
	r := f.reqs[0]
	if r.Method != "GET" || r.URL.Path != "/rest/api/2/issue/PROJ-412" || r.URL.Query().Get("fields") != "summary,description" {
		t.Errorf("request = %s %s", r.Method, r.URL)
	}
}

func TestJiraThroughTheRegistryTurnsTheKeyIntoATicketID(t *testing.T) {
	f := newFakeJira(t)
	reg := ticket.NewRegistry()
	reg.Register("jira", f.source("me@acme.com"))
	seed, err := reg.Fetch(context.Background(), "jira", "PROJ-412")
	if err != nil || seed.ID != "proj-412" || seed.Title != "Dashboard for users" {
		t.Errorf("seed = %+v, err %v", seed, err)
	}
}

func TestJiraAuthDependsOnWhetherAnEmailIsConfigured(t *testing.T) {
	f := newFakeJira(t)
	f.source("me@acme.com").Fetch(context.Background(), "PROJ-1")
	user, pass, ok := f.reqs[0].BasicAuth()
	if !ok || user != "me@acme.com" || pass != jiraToken {
		t.Errorf("cloud auth = %q %q %v", user, pass, ok)
	}
	f.source("").Fetch(context.Background(), "PROJ-1")
	if got := f.reqs[1].Header.Get("Authorization"); got != "Bearer "+jiraToken {
		t.Errorf("server auth = %q", got)
	}
}

func TestJiraOnlyEverReads(t *testing.T) {
	f := newFakeJira(t)
	for _, status := range []int{200, 404, 401, 500} {
		f.status = status
		f.source("me@acme.com").Fetch(context.Background(), "PROJ-1")
	}
	for _, r := range f.reqs {
		if r.Method != http.MethodGet {
			t.Errorf("a %s request was sent; rig must never write to Jira", r.Method)
		}
	}
}

func TestJiraErrorsAreClearAndNeverContainTheCredential(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
		is     error
	}{
		{"not found", 404, `{"errorMessages":["Issue does not exist"]}`, "PROJ-9 does not exist", ticket.ErrNotFound},
		{"unauthorized", 401, ``, "rejected the credentials (401); check the JIRA_API_TOKEN secret and jira.email", nil},
		{"forbidden", 403, ``, "may not read PROJ-9 (403)", nil},
		{"rate limited", 429, ``, "rate limiting", nil},
		{"a server error with messages", 500, `{"errorMessages":["db down"],"errors":{"x":"bad x"}}`, "answered 500: db down; bad x", nil},
		{"a bare error", 502, `<html>`, "answered 502", nil},
		{"not an issue", 200, `[]`, "not an issue", nil},
		{"no summary", 200, `{"key":"PROJ-9","fields":{"summary":"  "}}`, "has no summary", ticket.ErrUnusable},
	}
	for _, tt := range tests {
		f := newFakeJira(t)
		f.status, f.body = tt.status, tt.body
		_, err := f.source("me@acme.com").Fetch(context.Background(), "PROJ-9")
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
			continue
		}
		if tt.is != nil && !errors.Is(err, tt.is) {
			t.Errorf("%s: err = %v, want errors.Is %v", tt.name, err, tt.is)
		}
		if strings.Contains(err.Error(), jiraToken) {
			t.Errorf("%s: the credential is in the error", tt.name)
		}
	}
	// without an email the 401 hint says Cloud needs one
	f := newFakeJira(t)
	f.status = 401
	if _, err := f.source("").Fetch(context.Background(), "PROJ-9"); err == nil || !strings.Contains(err.Error(), "Atlassian Cloud needs jira.email") {
		t.Errorf("err = %v", err)
	}
}

func TestJiraRejectsBadKeysAndBadConfigWithoutAskingJira(t *testing.T) {
	f := newFakeJira(t)
	for _, ref := range []string{"", "412", "PROJ", "PROJ-", "PROJ-x", "../PROJ-1", "PROJ-1/../x", "a b-1", "PROJ-1?x=y"} {
		if _, err := f.source("e").Fetch(context.Background(), ref); !errors.Is(err, ticket.ErrUnusable) {
			t.Errorf("%q: err = %v, want unusable", ref, err)
		}
	}
	if len(f.reqs) != 0 {
		t.Errorf("%d requests were made for bad keys", len(f.reqs))
	}
	tok := func() (string, error) { return "t", nil }
	for _, base := range []string{"", "not a url", "http://jira.acme.com", "ftp://x", "https://"} {
		_, err := ticket.Jira{BaseURL: base, Token: tok}.Fetch(context.Background(), "PROJ-1")
		if !errors.Is(err, ticket.ErrNotConfigured) {
			t.Errorf("base %q: err = %v, want not configured", base, err)
		}
	}
	if _, err := (ticket.Jira{BaseURL: f.srv.URL, Token: func() (string, error) { return "", errors.New("secret JIRA_API_TOKEN not found") }}).Fetch(context.Background(), "PROJ-1"); err == nil || !strings.Contains(err.Error(), "JIRA_API_TOKEN not found") {
		t.Errorf("missing credential: %v", err)
	}
}

func TestJiraKeepsASubpathBaseURL(t *testing.T) {
	f := newFakeJira(t)
	j := ticket.Jira{BaseURL: f.srv.URL + "/jira/", Token: func() (string, error) { return "t", nil }}
	seed, err := j.Fetch(context.Background(), "PROJ-1")
	if err != nil || f.reqs[0].URL.Path != "/jira/rest/api/2/issue/PROJ-1" || seed.URL != f.srv.URL+"/jira/browse/PROJ-1" {
		t.Errorf("path %s, seed %+v, err %v", f.reqs[0].URL.Path, seed, err)
	}
}

func TestJiraScrubsTheCredentialFromTransportErrors(t *testing.T) {
	leaky := doerFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("proxy echoed " + r.Header.Get("Authorization"))
	})
	j := ticket.Jira{BaseURL: "https://acme.atlassian.net", Token: func() (string, error) { return jiraToken, nil }, HTTP: leaky}
	_, err := j.Fetch(context.Background(), "PROJ-1")
	if err == nil || strings.Contains(err.Error(), jiraToken) || !strings.Contains(err.Error(), "[credential]") {
		t.Errorf("err = %v", err)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestJiraAnIssueWithoutADescriptionIsFine(t *testing.T) {
	f := newFakeJira(t)
	f.body = `{"key":"PROJ-2","fields":{"summary":"Only a title","description":null}}`
	seed, err := f.source("e").Fetch(context.Background(), "PROJ-2")
	if err != nil || seed.Description != "" || seed.Title != "Only a title" {
		t.Errorf("seed = %+v, err %v", seed, err)
	}
}

func TestJiraMarkupConversion(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"headings", "h1. Big\nh3. Small", "# Big\n### Small"},
		{"bold and monospace", "A *bold* word and {{mono}} text", "A **bold** word and `mono` text"},
		{"not bold", "2 * 3 * 4 and a*b*c", "2 * 3 * 4 and a*b*c"},
		{"links", "See [the docs|https://x.example/d] and [https://y.example]", "See [the docs](https://x.example/d) and <https://y.example>"},
		{"nested bullets", "* a\n** b\n*** c\n* d", "- a\n  - b\n    - c\n- d"},
		{"numbered", "# one\n## sub\n# two", "1. one\n   1. sub\n1. two"},
		{"code block with a language", "{code:java}\nint x = *1*;\n{code}", "```java\nint x = *1*;\n```"},
		{"noformat", "{noformat}\nraw *text*\n{noformat}", "```\nraw *text*\n```"},
		{"one-line code", "{code}x = 1{code}", "```\nx = 1\n```"},
		{"quote block", "{quote}\nsaid *this*\n{quote}", "> said **this**"},
		{"block quote line", "bq. be careful", "> be careful"},
		{"table", "||a||b||\n|1|2|", "| a | b |\n| --- | --- |\n| 1 | 2 |"},
		{"table without a header", "|1|2|", "|  |  |\n| --- | --- |\n| 1 | 2 |"},
		{"rule", "a\n----\nb", "a\n---\nb"},
		{"strikethrough", "this is -gone- now", "this is ~~gone~~ now"},
		{"colour markup is dropped", "{color:red}danger{color} zone", "danger zone"},
		{"crlf", "h2. T\r\nline", "## T\nline"},
		{"unknown markup stays", "{panel}x{panel}", "{panel}x{panel}"},
		{"empty", "", ""},
		{"unterminated code runs to the end", "{code}\na\nb", "```\na\nb\n```"},
	}
	for _, tt := range tests {
		if got := ticket.JiraMarkupToMarkdown(tt.in); got != tt.want {
			t.Errorf("%s:\n in  %q\n got %q\n want %q", tt.name, tt.in, got, tt.want)
		}
	}
}
