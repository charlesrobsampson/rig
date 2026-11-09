package summary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Claude asks the Anthropic API. The ticket's content leaves this machine, so Remote is
// always true and every surface that shows a summary says so.
type Claude struct {
	endpoint *url.URL
	model    string
	key      func() (string, error)
	http     HTTPDoer
	timeout  time.Duration
}

const (
	defaultClaudeEndpoint = "https://api.anthropic.com"
	// DefaultClaudeModel is used when summarizer.model is not set: a small, fast model
	// is plenty for a five-bullet note.
	DefaultClaudeModel = "claude-haiku-5-5"
	anthropicVersion   = "2023-06-01"
	// claudeMaxTokens bounds the answer; the prompt asks for five short bullets.
	claudeMaxTokens = 400
)

// NewClaude builds the provider. key returns the API key each time it is needed, so a
// key added to the Keychain later takes effect without a restart; it is never logged and
// never appears in an error.
func NewClaude(endpoint, model string, opts ProviderOptions) (*Claude, error) {
	if opts.APIKey == nil {
		return nil, errors.New("the claude summarizer needs a way to find its API key")
	}
	if model == "" {
		model = DefaultClaudeModel
	}
	if endpoint == "" {
		endpoint = defaultClaudeEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return nil, fmt.Errorf("summarizer.endpoint %q must be an https URL (http is only allowed for this machine)", endpoint)
	}
	c := &Claude{endpoint: u, model: model, key: opts.APIKey, http: opts.HTTP, timeout: opts.Timeout}
	if c.timeout == 0 {
		c.timeout = 2 * time.Minute
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: c.timeout}
	}
	return c, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *Claude) Name() string  { return "claude" }
func (c *Claude) Model() string { return c.model }
func (c *Claude) Remote() bool  { return true }

type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Claude) Summarize(ctx context.Context, in Input) (string, error) {
	key, err := c.key()
	if err != nil {
		return "", fmt.Errorf("cannot get the Anthropic API key: %w", err)
	}
	if strings.TrimSpace(key) == "" {
		return "", errors.New("the Anthropic API key is empty")
	}
	// the request is exactly the prompt built from the ticket's own content: no tools,
	// no files, nothing else from this machine
	body, err := json.Marshal(messagesRequest{Model: c.model, MaxTokens: claudeMaxTokens, Messages: []message{{Role: "user", Content: Prompt(in)}}})
	if err != nil {
		return "", err
	}
	u := *c.endpoint
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("the Anthropic API at %s: %w", c.endpoint.Host, scrub(err, key))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read the Anthropic API's answer: %w", err)
	}
	var out messagesResponse
	_ = json.Unmarshal(raw, &out)

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("the Anthropic API refused the key (%d); check ANTHROPIC_API_KEY%s", resp.StatusCode, apiDetail(out))
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", fmt.Errorf("the Anthropic API is rate limiting this key (429)%s", apiDetail(out))
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("the Anthropic API answered %d%s", resp.StatusCode, apiDetail(out))
	}
	var text strings.Builder
	for _, part := range out.Content {
		if part.Type == "text" {
			text.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", errors.New("the Anthropic API returned an empty summary")
	}
	return strings.TrimSpace(text.String()), nil
}

func apiDetail(r messagesResponse) string {
	if r.Error != nil && r.Error.Message != "" {
		return ": " + r.Error.Message
	}
	return ""
}

// scrub removes the key from an error's text, in case a transport echoes headers.
func scrub(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[key]"))
}
