// Package inject types messages into running agents (docs/spikes/03-tmux.md §3).
// Typing into an agent is dangerous in one case: a permission prompt, where the Enter
// that submits a message would approve whatever the agent asked to do. So a message is
// refused outright while the agent is blocked, and the state is checked again between
// the text and the Enter.
package inject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

// Readiness says whether an agent's interface is up (the supervisor knows from hooks).
type Readiness interface {
	Ready(ticket, service string) bool
}

type Options struct {
	Dir       string // the tickets directory
	States    *state.Manager
	Backend   terminal.Backend
	Readiness Readiness
	Clock     clock.Clock // nil means the real clock
	// Pause is the wait between typing the text and pressing Enter, so the agent's
	// interface registers the text first (default 400ms).
	Pause time.Duration
	// ReadyTimeout is how long a message waits for a starting agent (default 30s).
	ReadyTimeout time.Duration
	// Poll is how often the wait looks again (default 250ms).
	Poll time.Duration
}

// maxText bounds one message; this is a nudge, not a file transfer.
const maxText = 16 * 1024

type Injector struct {
	opts Options

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(o Options) *Injector {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Pause == 0 {
		o.Pause = 400 * time.Millisecond
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 30 * time.Second
	}
	if o.Poll == 0 {
		o.Poll = 250 * time.Millisecond
	}
	return &Injector{opts: o, locks: map[string]*sync.Mutex{}}
}

// logEntry is one line of messages.jsonl in the ticket directory.
type logEntry struct {
	At        time.Time `json:"at"`
	Service   string    `json:"service"`
	From      string    `json:"from"`
	Text      string    `json:"text"`
	Delivered bool      `json:"delivered"`
	Reason    string    `json:"reason,omitempty"`
}

func invalid(format string, args ...any) *api.Error {
	return &api.Error{Code: api.CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

// Send delivers a message. from says who sent it for the log ("human", or "rig" for
// notices rig sends itself). Every attempt is logged, refused ones with the reason.
func (i *Injector) Send(ctx context.Context, p api.AgentMessageParams, from string) (api.AgentMessageResult, error) {
	switch {
	case strings.TrimSpace(p.Text) == "":
		return api.AgentMessageResult{}, invalid("a message needs some text")
	case len(p.Text) > maxText:
		return api.AgentMessageResult{}, invalid("a message can be at most %d bytes, got %d", maxText, len(p.Text))
	}
	if err := domain.ValidateTicketID(p.Ticket); err != nil {
		return api.AgentMessageResult{}, invalid("%v", err)
	}
	statePath := filepath.Join(i.opts.Dir, p.Ticket, "state.json")
	if _, err := os.Stat(statePath); err != nil {
		return api.AgentMessageResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q not found", p.Ticket)}
	}

	unlock := i.lock(p.Ticket + "/" + p.Service)
	defer unlock()

	sent, err := i.deliver(ctx, statePath, p)
	i.record(p, from, err)
	if err != nil {
		return api.AgentMessageResult{}, err
	}
	return api.AgentMessageResult{Service: p.Service, Sent: sent}, nil
}

func (i *Injector) lock(key string) (unlock func()) {
	i.mu.Lock()
	l, ok := i.locks[key]
	if !ok {
		l = &sync.Mutex{}
		i.locks[key] = l
	}
	i.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (i *Injector) deliver(ctx context.Context, statePath string, p api.AgentMessageParams) (string, error) {
	pane, err := i.waitUntilReady(ctx, statePath, p)
	if err != nil {
		return "", err
	}
	text := "[rig] " + Escape(p.Text)
	if err := i.opts.Backend.SendText(ctx, pane, text); err != nil {
		return "", i.backendError(p.Service, err)
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-i.opts.Clock.After(i.opts.Pause):
	}
	// Something may have asked for permission while the text was going in; Enter
	// would answer it.
	if _, err := i.check(ctx, statePath, p); err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == api.CodeBlocked {
			apiErr.Message += "; the text was typed but not submitted and may be waiting in its input"
		}
		return "", err
	}
	if err := i.opts.Backend.Submit(ctx, pane); err != nil {
		return "", i.backendError(p.Service, err)
	}
	return text, nil
}

// waitUntilReady holds a message for an agent that is still starting. Text typed
// before its interface is up is lost, so it must not be sent early.
func (i *Injector) waitUntilReady(ctx context.Context, statePath string, p api.AgentMessageParams) (terminal.PaneID, error) {
	deadline := i.opts.Clock.After(i.opts.ReadyTimeout)
	for {
		ready, err := i.check(ctx, statePath, p)
		if err != nil {
			return "", err
		}
		if ready.up {
			return ready.pane, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("the agent of service %q has not reported ready within %s; try again in a moment", p.Service, i.opts.ReadyTimeout)}
		case <-i.opts.Clock.After(i.opts.Poll):
		}
	}
}

type readiness struct {
	pane terminal.PaneID
	up   bool
}

// check decides whether typing is safe right now. It returns an error when it is
// not and will not become so on its own (no agent, dead pane, blocked).
func (i *Injector) check(ctx context.Context, statePath string, p api.AgentMessageParams) (readiness, error) {
	st, err := i.opts.States.Load(statePath)
	if err != nil {
		return readiness{}, err
	}
	svc, ok := st.Services[p.Service]
	if !ok {
		return readiness{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no service %q", p.Ticket, p.Service)}
	}
	if svc.Agent.PaneID == "" {
		return readiness{}, &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("service %q has no agent running; start the ticket first", p.Service)}
	}
	pane, err := i.opts.Backend.Get(ctx, terminal.PaneID(svc.Agent.PaneID))
	if err != nil {
		return readiness{}, i.backendError(p.Service, err)
	}
	if !pane.Alive {
		return readiness{}, &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("the agent of service %q has exited", p.Service)}
	}
	// Either signal is enough to refuse: the hook state may lag a prompt that just
	// appeared, and the screen guess may not recognize one.
	if svc.Agent.State == domain.AgentBlocked || pane.Heuristic == domain.AgentBlocked {
		return readiness{}, &api.Error{Code: api.CodeBlocked, Message: fmt.Sprintf("the agent of service %q is waiting on a permission prompt; answer it first (rig attach), nothing was typed", p.Service)}
	}
	if svc.Agent.State == domain.AgentExited {
		return readiness{}, &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("the agent of service %q has exited", p.Service)}
	}
	// After a daemon restart the in-memory ready flag is gone, but a recorded idle or
	// working state could only have come from the agent's own hooks.
	up := i.opts.Readiness.Ready(p.Ticket, p.Service) || svc.Agent.State == domain.AgentIdle || svc.Agent.State == domain.AgentWorking
	return readiness{pane: terminal.PaneID(svc.Agent.PaneID), up: up}, nil
}

func (i *Injector) backendError(service string, err error) error {
	switch {
	case errors.Is(err, terminal.ErrNotFound):
		return &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("the agent of service %q is no longer running", service)}
	case errors.Is(err, terminal.ErrUnavailable):
		return &api.Error{Code: api.CodeUnavailable, Message: err.Error()}
	}
	return err
}

// record appends to the ticket's message log. A log that cannot be written never
// fails the message: it has already been delivered or refused.
func (i *Injector) record(p api.AgentMessageParams, from string, outcome error) {
	e := logEntry{At: i.opts.Clock.Now().UTC(), Service: p.Service, From: from, Text: p.Text, Delivered: outcome == nil}
	if outcome != nil {
		e.Reason = outcome.Error()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(i.opts.Dir, p.Ticket, "messages.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// Escape makes text safe to type into a terminal: newlines and tabs stay, every other
// control character (an Escape that could start a key sequence or end a paste, a
// carriage return that would press Enter, Ctrl-C) is written out as \xNN, and so are
// bytes that are not valid UTF-8.
func Escape(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, text[i])
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}
