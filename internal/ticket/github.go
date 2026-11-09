package ticket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitHub reads one issue from GitHub (or GitHub Enterprise). Like the Jira source it only
// ever sends GET requests: rig takes the title and body and writes nothing back.
type GitHub struct {
	// APIURL is the API root: https://api.github.com by default, or
	// https://github.example.com/api/v3 for GitHub Enterprise.
	APIURL string
	// Token finds the credential when it is needed. found is false when there is none;
	// the request is then anonymous, which works for public repositories (with GitHub's
	// low anonymous rate limit). The token is never logged or put in an error.
	Token func() (token string, found bool, err error)
	HTTP  HTTPDoer
}

// owner/repo#123: GitHub's own names allow letters, digits, dots, dashes and underscores.
var githubRefRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})#([0-9]{1,9})$`)

const defaultGitHubAPI = "https://api.github.com"

func (g GitHub) Fetch(ctx context.Context, ref string) (Seed, error) {
	m := githubRefRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil || m[2] == "." || m[2] == ".." {
		return Seed{}, fmt.Errorf("%w: %q is not a GitHub issue reference (expected owner/repo#123)", ErrUnusable, ref)
	}
	owner, repo, number := m[1], m[2], m[3]
	n, _ := strconv.Atoi(number)
	if n < 1 {
		return Seed{}, fmt.Errorf("%w: issue numbers start at 1", ErrUnusable)
	}
	base, err := g.base()
	if err != nil {
		return Seed{}, err
	}
	token, haveToken := "", false
	if g.Token != nil {
		if token, haveToken, err = g.Token(); err != nil {
			return Seed{}, fmt.Errorf("cannot get the GitHub credential: %w", err)
		}
	}

	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/issues/" + number
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Seed{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if haveToken && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	doer := g.HTTP
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return Seed{}, fmt.Errorf("GitHub at %s: %w", base.Host, scrubSecret(err, token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Seed{}, fmt.Errorf("read GitHub's answer: %w", err)
	}

	name := fmt.Sprintf("%s/%s#%d", owner, repo, n)
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		hint := ""
		if !haveToken || token == "" {
			hint = "; if the repository is private, add the GITHUB_TOKEN secret"
		}
		return Seed{}, fmt.Errorf("%w: %s does not exist, or this account cannot see it%s", ErrNotFound, name, hint)
	case resp.StatusCode == http.StatusGone:
		return Seed{}, fmt.Errorf("%w: %s was deleted, or the repository has issues turned off (410)", ErrNotFound, name)
	case resp.StatusCode == http.StatusUnauthorized:
		return Seed{}, errors.New("GitHub rejected the credentials (401); check the GITHUB_TOKEN secret")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.StatusCode == http.StatusTooManyRequests {
			hint := ""
			if !haveToken {
				hint = "; add a GITHUB_TOKEN secret for a higher limit"
			}
			return Seed{}, fmt.Errorf("GitHub is rate limiting this client (%d)%s", resp.StatusCode, hint)
		}
		return Seed{}, fmt.Errorf("GitHub says this account may not read %s (403)%s", name, githubMessage(raw))
	default:
		return Seed{}, fmt.Errorf("GitHub answered %d%s", resp.StatusCode, githubMessage(raw))
	}

	var issue struct {
		Number      int             `json:"number"`
		Title       string          `json:"title"`
		Body        *string         `json:"body"`
		HTMLURL     string          `json:"html_url"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Seed{}, fmt.Errorf("GitHub's answer for %s is not an issue: %w", name, err)
	}
	if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" {
		return Seed{}, fmt.Errorf("%w: %s is a pull request, not an issue; seed tickets from issues", ErrUnusable, name)
	}
	if strings.TrimSpace(issue.Title) == "" {
		return Seed{}, fmt.Errorf("%w: %s has no title", ErrUnusable, name)
	}
	seed := Seed{
		ID:    owner + "-" + repo + "-" + number,
		Title: strings.TrimSpace(issue.Title),
		URL:   issue.HTMLURL,
	}
	if seed.URL == "" {
		seed.URL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)
	}
	if issue.Body != nil {
		seed.Description = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(*issue.Body, "\r\n", "\n"), "\r", "\n"))
	}
	return seed, nil
}

func (g GitHub) base() (*url.URL, error) {
	raw := g.APIURL
	if strings.TrimSpace(raw) == "" {
		raw = defaultGitHubAPI
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, fmt.Errorf("%w: github.api_url %q must be an https URL", ErrNotConfigured, raw)
	}
	return u, nil
}

func githubMessage(raw []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Message == "" {
		return ""
	}
	return ": " + body.Message
}
