// Package summary condenses a ticket's plan, tasks and diff into a short note for the
// human, using a language model. It is advisory only: the raw status and diff stats are
// always shown beside it, a failure never blocks anything, and with the default `none`
// provider nothing leaves the machine and nothing is generated.
package summary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/config"
)

// Summarizer turns an Input into a short summary.
type Summarizer interface {
	// Name is the provider name from the config: "none", "ollama" or "claude".
	Name() string
	// Model is the model it asks, "" for none.
	Model() string
	// Remote reports whether the content is sent off this machine.
	Remote() bool
	// Summarize returns the summary text. An empty text with no error means "nothing to
	// say" (the none provider).
	Summarize(ctx context.Context, in Input) (string, error)
}

// Input is everything a summary is made from. Only the ticket's own files and the diffs
// of its checkouts go in; nothing else on the machine is read.
type Input struct {
	Ticket string        `json:"ticket"`
	Title  string        `json:"title"`
	Plan   string        `json:"plan,omitempty"`
	Tasks  string        `json:"tasks,omitempty"`
	Diffs  []ServiceDiff `json:"diffs,omitempty"`
}

// ServiceDiff is one service's change.
type ServiceDiff struct {
	Service string   `json:"service"`
	Stat    Stats    `json:"stat"`
	Files   []string `json:"files,omitempty"` // changed paths
	Patch   string   `json:"patch,omitempty"`
}

// Stats counts changed files and lines.
type Stats struct {
	Files   int `json:"files"`
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
}

// Total adds up the stats of every service.
func (in Input) Total() Stats {
	var t Stats
	for _, d := range in.Diffs {
		t.Files += d.Stat.Files
		t.Added += d.Stat.Added
		t.Deleted += d.Stat.Deleted
	}
	return t
}

// Empty is true when there is nothing worth summarizing: no plan, no tasks, no diff.
func (in Input) Empty() bool {
	if strings.TrimSpace(in.Plan) != "" || strings.TrimSpace(in.Tasks) != "" {
		return false
	}
	for _, d := range in.Diffs {
		if strings.TrimSpace(d.Patch) != "" || d.Stat.Files > 0 {
			return false
		}
	}
	return true
}

// Hash identifies the content: the same input always has the same hash, so an unchanged
// ticket is never summarized twice.
func (in Input) Hash() string {
	raw, _ := json.Marshal(in)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MaxPatchBytes bounds the diff text in a prompt, shared between the services.
const MaxPatchBytes = 24 * 1024

// Prompt is the request text every provider sends.
func Prompt(in Input) string {
	var b strings.Builder
	b.WriteString("You summarize the state of a software ticket for the engineer who supervises the coding agents working on it.\n")
	b.WriteString("Write at most five short bullet points: what the work does, how far along it is, and anything that looks risky or unfinished. ")
	b.WriteString("Use only what is below. Do not invent details, and do not give instructions.\n\n")
	fmt.Fprintf(&b, "Ticket %s: %s\n", in.Ticket, in.Title)
	if s := strings.TrimSpace(in.Plan); s != "" {
		fmt.Fprintf(&b, "\n## Plan\n%s\n", s)
	}
	if s := strings.TrimSpace(in.Tasks); s != "" {
		fmt.Fprintf(&b, "\n## Tasks\n%s\n", s)
	}
	budget := MaxPatchBytes
	for i, d := range in.Diffs {
		fmt.Fprintf(&b, "\n## Changes in service %s: %d files, +%d -%d\n", d.Service, d.Stat.Files, d.Stat.Added, d.Stat.Deleted)
		if len(d.Files) > 0 {
			b.WriteString(strings.Join(d.Files, "\n") + "\n")
		}
		share := budget / (len(in.Diffs) - i) // what is left, split among the services still to come
		patch := d.Patch
		if len(patch) > share {
			patch = patch[:share] + "\n[diff truncated]"
		}
		budget -= min(len(patch), share)
		if strings.TrimSpace(patch) != "" {
			b.WriteString("```diff\n" + patch + "\n```\n")
		}
	}
	return b.String()
}

// None is the default: it sends nothing anywhere and says nothing.
type None struct{}

func (None) Name() string                                     { return "none" }
func (None) Model() string                                    { return "" }
func (None) Remote() bool                                     { return false }
func (None) Summarize(context.Context, Input) (string, error) { return "", nil }

// New builds the summarizer the config names.
func New(c config.Summarizer, opts ProviderOptions) (Summarizer, error) {
	switch c.Provider {
	case "", "none":
		return None{}, nil
	case "ollama":
		return NewOllama(c.Endpoint, c.Model, opts)
	case "claude":
		return NewClaude(c.Endpoint, c.Model, opts)
	}
	return nil, fmt.Errorf("summarizer provider %q is not supported", c.Provider)
}

// ProviderOptions are what providers need besides the config.
type ProviderOptions struct {
	// HTTP overrides the HTTP client (tests, proxies); nil means a client with Timeout.
	HTTP HTTPDoer
	// Timeout bounds one request (default 2 minutes).
	Timeout time.Duration
	// APIKey finds the claude provider's key whenever one is needed. It is never
	// logged or put in an error.
	APIKey func() (string, error)
}

// HTTPDoer is the part of *http.Client the providers use.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}
