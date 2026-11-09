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
	"strings"
	"time"
)

// ErrNotConfigured is a source that is named but has no settings to work with.
var ErrNotConfigured = errors.New("ticket source is not configured")

// HTTPDoer is the part of *http.Client the sources use.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Jira reads one issue from a Jira server. It only ever sends GET requests: rig takes a
// ticket's title and description and writes nothing back.
type Jira struct {
	// BaseURL is the Jira site, like https://acme.atlassian.net.
	BaseURL string
	// Email, when set, makes the token an Atlassian Cloud API token used with basic
	// auth; without it the token is sent as a bearer token (a Server/Data Center
	// personal access token).
	Email string
	// Token finds the credential when it is needed. It is never logged or put in an error.
	Token func() (string, error)
	HTTP  HTTPDoer
}

var jiraKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)

func (j Jira) Fetch(ctx context.Context, ref string) (Seed, error) {
	key := strings.ToUpper(strings.TrimSpace(ref))
	if !jiraKeyRe.MatchString(key) {
		return Seed{}, fmt.Errorf("%w: %q is not a Jira issue key (expected something like PROJ-412)", ErrUnusable, ref)
	}
	base, err := j.base()
	if err != nil {
		return Seed{}, err
	}
	token, err := j.Token()
	if err != nil {
		return Seed{}, fmt.Errorf("cannot get the Jira credential: %w", err)
	}

	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/rest/api/2/issue/" + url.PathEscape(key)
	u.RawQuery = url.Values{"fields": {"summary,description"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Seed{}, err
	}
	req.Header.Set("Accept", "application/json")
	if j.Email != "" {
		req.SetBasicAuth(j.Email, token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	doer := j.HTTP
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return Seed{}, fmt.Errorf("Jira at %s: %w", base.Host, scrubSecret(err, token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Seed{}, fmt.Errorf("read Jira's answer: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Seed{}, fmt.Errorf("%w: issue %s does not exist in %s, or this account cannot see it", ErrNotFound, key, base.Host)
	case http.StatusUnauthorized:
		return Seed{}, fmt.Errorf("Jira rejected the credentials (401); check the JIRA_API_TOKEN secret%s", emailHint(j.Email))
	case http.StatusForbidden:
		return Seed{}, fmt.Errorf("Jira says this account may not read %s (403)", key)
	case http.StatusTooManyRequests:
		return Seed{}, errors.New("Jira is rate limiting this account (429); try again in a minute")
	default:
		return Seed{}, fmt.Errorf("Jira answered %d%s", resp.StatusCode, jiraMessages(raw))
	}

	var issue struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string  `json:"summary"`
			Description *string `json:"description"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Seed{}, fmt.Errorf("Jira's answer for %s is not an issue: %w", key, err)
	}
	if strings.TrimSpace(issue.Fields.Summary) == "" {
		return Seed{}, fmt.Errorf("%w: issue %s has no summary", ErrUnusable, key)
	}
	seed := Seed{
		ID:    key,
		Title: strings.TrimSpace(issue.Fields.Summary),
		URL:   strings.TrimRight(base.String(), "/") + "/browse/" + key,
	}
	if issue.Fields.Description != nil {
		seed.Description = JiraMarkupToMarkdown(*issue.Fields.Description)
	}
	return seed, nil
}

func (j Jira) base() (*url.URL, error) {
	if strings.TrimSpace(j.BaseURL) == "" {
		return nil, fmt.Errorf("%w: set jira.base_url in the config (for example https://acme.atlassian.net)", ErrNotConfigured)
	}
	u, err := url.Parse(j.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, fmt.Errorf("%w: jira.base_url %q must be an https URL", ErrNotConfigured, j.BaseURL)
	}
	return u, nil
}

func isLoopbackHost(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

func emailHint(email string) string {
	if email == "" {
		return " (no jira.email is set, so it is sent as a personal access token; Atlassian Cloud needs jira.email too)"
	}
	return " and jira.email"
}

// jiraMessages pulls the error messages out of a Jira error body, if it has any.
func jiraMessages(raw []byte) string {
	var body struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	msgs := append([]string(nil), body.ErrorMessages...)
	for _, m := range body.Errors {
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return ""
	}
	return ": " + strings.Join(msgs, "; ")
}

// scrubSecret removes a credential from an error's text, in case a transport echoes it.
func scrubSecret(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[credential]"))
}
