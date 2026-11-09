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

const ghToken = "ghp_secretTOKEN0123456789"

type fakeGitHub struct {
	mu      sync.Mutex
	srv     *httptest.Server
	reqs    []*http.Request
	status  int
	body    string
	headers map[string]string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{status: 200, body: `{"number":123,"title":" Add a locale header ","body":"## Why\r\nBecause *users*.\r\n\r\n- one\r\n- two","html_url":"https://github.com/acme/api/issues/123"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		status, body, headers := f.status, f.body, f.headers
		f.mu.Unlock()
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) source(token string, found bool) ticket.GitHub {
	return ticket.GitHub{APIURL: f.srv.URL, Token: func() (string, bool, error) { return token, found, nil }}
}

func TestGitHubSeedsTitleBodyAndURL(t *testing.T) {
	f := newFakeGitHub(t)
	seed, err := f.source(ghToken, true).Fetch(context.Background(), "acme/api#123")
	if err != nil {
		t.Fatal(err)
	}
	if seed.ID != "acme-api-123" || seed.Title != "Add a locale header" || seed.URL != "https://github.com/acme/api/issues/123" {
		t.Errorf("seed = %+v", seed)
	}
	if want := "## Why\nBecause *users*.\n\n- one\n- two"; seed.Description != want {
		t.Errorf("description = %q, want %q", seed.Description, want)
	}
	r := f.reqs[0]
	if r.Method != "GET" || r.URL.Path != "/repos/acme/api/issues/123" || r.Header.Get("Authorization") != "Bearer "+ghToken ||
		r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" {
		t.Errorf("request = %s %s %v", r.Method, r.URL, r.Header)
	}
}

func TestGitHubThroughTheRegistryGivesAValidTicketID(t *testing.T) {
	f := newFakeGitHub(t)
	reg := ticket.NewRegistry()
	reg.Register("github", f.source("", false))
	seed, err := reg.Fetch(context.Background(), "github", "Acme/My_Repo.js#123")
	if err != nil || seed.ID != "acme-my-repo-js-123" {
		t.Errorf("seed = %+v, err %v", seed, err)
	}
}

func TestGitHubWithoutACredentialIsAnonymous(t *testing.T) {
	f := newFakeGitHub(t)
	if _, err := f.source("", false).Fetch(context.Background(), "acme/api#1"); err != nil {
		t.Fatal(err)
	}
	if got := f.reqs[0].Header.Get("Authorization"); got != "" {
		t.Errorf("an anonymous request sent Authorization %q", got)
	}
	// no Token function at all is the same
	if _, err := (ticket.GitHub{APIURL: f.srv.URL}).Fetch(context.Background(), "acme/api#1"); err != nil {
		t.Errorf("without a token source: %v", err)
	}
}

func TestGitHubPullRequestsAreRejectedClearly(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = `{"number":7,"title":"Fix it","body":"x","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/7"}}`
	_, err := f.source("", false).Fetch(context.Background(), "acme/api#7")
	if !errors.Is(err, ticket.ErrUnusable) || !strings.Contains(err.Error(), "acme/api#7 is a pull request, not an issue") {
		t.Errorf("err = %v", err)
	}
	f.body = `{"number":8,"title":"An issue","pull_request":null}`
	if _, err := f.source("", false).Fetch(context.Background(), "acme/api#8"); err != nil {
		t.Errorf("a null pull_request field made an issue look like a PR: %v", err)
	}
}

func TestGitHubErrorsAreClearAndNeverContainTheCredential(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		token   bool
		want    string
		is      error
	}{
		{"not found, anonymous", 404, `{"message":"Not Found"}`, nil, false, "does not exist, or this account cannot see it; if the repository is private, add the GITHUB_TOKEN secret", ticket.ErrNotFound},
		{"not found, with a token", 404, `{"message":"Not Found"}`, nil, true, "does not exist, or this account cannot see it", ticket.ErrNotFound},
		{"gone", 410, `{"message":"This issue was deleted"}`, nil, true, "deleted, or the repository has issues turned off", ticket.ErrNotFound},
		{"bad credentials", 401, `{"message":"Bad credentials"}`, nil, true, "rejected the credentials (401); check the GITHUB_TOKEN secret", nil},
		{"rate limited with a token", 403, `{"message":"API rate limit exceeded"}`, map[string]string{"X-RateLimit-Remaining": "0"}, true, "rate limiting this client (403)", nil},
		{"rate limited anonymously", 403, `{}`, map[string]string{"X-RateLimit-Remaining": "0"}, false, "add a GITHUB_TOKEN secret for a higher limit", nil},
		{"too many requests", 429, `{}`, nil, true, "rate limiting this client (429)", nil},
		{"forbidden", 403, `{"message":"Resource not accessible by personal access token"}`, nil, true, "may not read acme/api#5 (403): Resource not accessible", nil},
		{"a server error", 502, `{"message":"Server Error"}`, nil, true, "answered 502: Server Error", nil},
		{"not an issue", 200, `[]`, nil, true, "not an issue", nil},
		{"no title", 200, `{"number":5,"title":"  "}`, nil, true, "has no title", ticket.ErrUnusable},
	}
	for _, tt := range tests {
		f := newFakeGitHub(t)
		f.status, f.body, f.headers = tt.status, tt.body, tt.headers
		_, err := f.source(ghToken, tt.token).Fetch(context.Background(), "acme/api#5")
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
			continue
		}
		if tt.is != nil && !errors.Is(err, tt.is) {
			t.Errorf("%s: err = %v, want errors.Is %v", tt.name, err, tt.is)
		}
		if strings.Contains(err.Error(), ghToken) {
			t.Errorf("%s: the credential is in the error", tt.name)
		}
	}
}

func TestGitHubOnlyEverReads(t *testing.T) {
	f := newFakeGitHub(t)
	for _, status := range []int{200, 404, 401, 500} {
		f.status = status
		f.source(ghToken, true).Fetch(context.Background(), "acme/api#1")
	}
	for _, r := range f.reqs {
		if r.Method != http.MethodGet {
			t.Errorf("a %s request was sent; rig must never write to GitHub", r.Method)
		}
	}
}

func TestGitHubRejectsBadReferencesWithoutAsking(t *testing.T) {
	f := newFakeGitHub(t)
	for _, ref := range []string{"", "123", "#123", "acme/api", "acme/api#", "acme/api#x", "acme/api#0", "acme#1", "acme/api#1/extra",
		"../etc/passwd#1", "acme/../api#1", "acme/..#1", "a b/c#1", "acme/api#1?x=y", "-acme/api#1", "acme/api#9999999999"} {
		if _, err := f.source("", false).Fetch(context.Background(), ref); !errors.Is(err, ticket.ErrUnusable) {
			t.Errorf("%q: err = %v, want unusable", ref, err)
		}
	}
	if len(f.reqs) != 0 {
		t.Errorf("%d requests were made for bad references", len(f.reqs))
	}
}

func TestGitHubAPIURLMustBeSecureAndMayHaveASubpath(t *testing.T) {
	for _, base := range []string{"http://ghe.example.com/api/v3", "ftp://x", "https://", "nonsense"} {
		_, err := ticket.GitHub{APIURL: base}.Fetch(context.Background(), "acme/api#1")
		if !errors.Is(err, ticket.ErrNotConfigured) {
			t.Errorf("%q: err = %v, want not configured", base, err)
		}
	}
	f := newFakeGitHub(t)
	g := ticket.GitHub{APIURL: f.srv.URL + "/api/v3/"}
	if _, err := g.Fetch(context.Background(), "acme/api#1"); err != nil || f.reqs[0].URL.Path != "/api/v3/repos/acme/api/issues/1" {
		t.Errorf("path = %v, err %v", f.reqs[0].URL.Path, err)
	}
}

func TestGitHubCredentialProblemsAndTransportErrors(t *testing.T) {
	f := newFakeGitHub(t)
	broken := ticket.GitHub{APIURL: f.srv.URL, Token: func() (string, bool, error) { return "", false, errors.New("keychain is locked") }}
	if _, err := broken.Fetch(context.Background(), "acme/api#1"); err == nil || !strings.Contains(err.Error(), "keychain is locked") || len(f.reqs) != 0 {
		t.Errorf("err = %v, requests %d", err, len(f.reqs))
	}
	leaky := doerFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("proxy echoed " + r.Header.Get("Authorization"))
	})
	g := ticket.GitHub{APIURL: "https://api.github.com", HTTP: leaky, Token: func() (string, bool, error) { return ghToken, true, nil }}
	_, err := g.Fetch(context.Background(), "acme/api#1")
	if err == nil || strings.Contains(err.Error(), ghToken) || !strings.Contains(err.Error(), "[credential]") {
		t.Errorf("err = %v", err)
	}
}

func TestGitHubAnIssueWithoutABodyIsFine(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = `{"number":2,"title":"Only a title","body":null}`
	seed, err := f.source("", false).Fetch(context.Background(), "acme/api#2")
	if err != nil || seed.Description != "" || seed.URL != "https://github.com/acme/api/issues/2" {
		t.Errorf("seed = %+v, err %v", seed, err)
	}
}
