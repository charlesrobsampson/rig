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

// Ollama asks a local Ollama server. Nothing leaves the machine unless the endpoint
// points elsewhere, which Remote reports.
type Ollama struct {
	endpoint *url.URL
	model    string
	http     HTTPDoer
	timeout  time.Duration
}

const defaultOllamaEndpoint = "http://localhost:11434"

func NewOllama(endpoint, model string, opts ProviderOptions) (*Ollama, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("summarizer.model is required for the ollama provider (for example \"llama3:8b\")")
	}
	if endpoint == "" {
		endpoint = defaultOllamaEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("summarizer.endpoint %q is not an http(s) URL", endpoint)
	}
	o := &Ollama{endpoint: u, model: model, http: opts.HTTP, timeout: opts.Timeout}
	if o.timeout == 0 {
		o.timeout = 2 * time.Minute
	}
	if o.http == nil {
		o.http = &http.Client{Timeout: o.timeout}
	}
	return o, nil
}

func (o *Ollama) Name() string  { return "ollama" }
func (o *Ollama) Model() string { return o.model }

// Remote is true unless the endpoint is this machine.
func (o *Ollama) Remote() bool {
	switch o.endpoint.Hostname() {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return false
	}
	return true
}

type generateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

type generateResponse struct {
	Response string `json:"response"`
	Error    string `json:"error"`
}

func (o *Ollama) Summarize(ctx context.Context, in Input) (string, error) {
	body, err := json.Marshal(generateRequest{Model: o.model, Prompt: Prompt(in), Stream: false, Options: map[string]any{"temperature": 0.2}})
	if err != nil {
		return "", err
	}
	u := *o.endpoint
	u.Path = strings.TrimRight(u.Path, "/") + "/api/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama at %s: %w", o.endpoint.Host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read ollama's answer: %w", err)
	}
	var out generateResponse
	_ = json.Unmarshal(raw, &out) // an error body may not be JSON; the status below still tells
	switch {
	case resp.StatusCode == http.StatusNotFound && strings.Contains(out.Error, "not found"):
		return "", fmt.Errorf("ollama does not have the model %q (%s); run: ollama pull %s", o.model, out.Error, o.model)
	case resp.StatusCode != http.StatusOK && out.Error != "":
		return "", fmt.Errorf("ollama answered %d: %s", resp.StatusCode, out.Error)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("ollama answered %d", resp.StatusCode)
	case out.Error != "":
		return "", fmt.Errorf("ollama: %s", out.Error)
	}
	text := strings.TrimSpace(out.Response)
	if text == "" {
		return "", errors.New("ollama returned an empty summary")
	}
	return text, nil
}
