// Package phasenotify tells running agents that their ticket moved to another phase,
// with the rules of the new one. Notices are typed in through the same injector as
// `rig msg`, so they obey its rules (never into an agent waiting on a prompt, logged in
// messages.jsonl). Unlike contract notices, a phase notice is not dropped when the agent
// is blocked or still starting: it is tried again, less often each time, until it is
// delivered, a newer move replaces it, or the agent is gone.
package phasenotify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
)

// Sender types a message into a service's agent; *inject.Injector is one.
type Sender interface {
	Send(ctx context.Context, p api.AgentMessageParams, from string) (api.AgentMessageResult, error)
}

type Options struct {
	Sender Sender
	Clock  clock.Clock // nil means the real clock
	Logger *slog.Logger
	// Retry is the first wait before trying a refused notice again (default 15s); each
	// later wait doubles, up to MaxRetry (default 5 minutes).
	Retry, MaxRetry time.Duration
	// Timeout bounds one attempt (default 2 minutes; an agent may be busy starting).
	Timeout time.Duration
}

type Notifier struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	pending map[string]*notice     // by ticket/service: the notice still to deliver
	order   map[string]*sync.Mutex // by ticket/service: one attempt at a time, in order
	wg      sync.WaitGroup
}

type notice struct {
	ticket, service, text string
	wait                  time.Duration
	timer                 clock.Timer
}

func New(o Options) *Notifier {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Retry == 0 {
		o.Retry = 15 * time.Second
	}
	if o.MaxRetry == 0 {
		o.MaxRetry = 5 * time.Minute
	}
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Notifier{opts: o, ctx: ctx, cancel: cancel, pending: map[string]*notice{}, order: map[string]*sync.Mutex{}}
}

// PhaseChanged delivers each service's notice. A notice still waiting from an earlier
// move is replaced: only the latest phase matters to the agent. It never blocks.
func (n *Notifier) PhaseChanged(ticket string, notices map[string]string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	for service, text := range notices {
		key := ticket + "/" + service
		if old := n.pending[key]; old != nil && old.timer != nil {
			old.timer.Stop()
		}
		nt := &notice{ticket: ticket, service: service, text: text}
		n.pending[key] = nt
		n.startLocked(key, nt)
	}
}

// Wait blocks until every attempt started so far has finished (tests use it).
func (n *Notifier) Wait() { n.wg.Wait() }

// Close drops every waiting notice and waits for attempts in flight to end.
func (n *Notifier) Close() {
	n.mu.Lock()
	n.closed = true
	for _, nt := range n.pending {
		if nt.timer != nil {
			nt.timer.Stop()
		}
	}
	n.pending = map[string]*notice{}
	n.mu.Unlock()
	n.cancel()
	n.wg.Wait()
}

func (n *Notifier) startLocked(key string, nt *notice) {
	lock, ok := n.order[key]
	if !ok {
		lock = &sync.Mutex{}
		n.order[key] = lock
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		lock.Lock()
		defer lock.Unlock()
		n.attempt(key, nt)
	}()
}

func (n *Notifier) attempt(key string, nt *notice) {
	if !n.current(key, nt) {
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, n.opts.Timeout)
	_, err := n.opts.Sender.Send(ctx, api.AgentMessageParams{Ticket: nt.ticket, Service: nt.service, Text: nt.text}, "rig")
	cancel()

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.pending[key] != nt {
		return
	}
	if err == nil || !worthRetrying(err) {
		delete(n.pending, key)
		if err != nil {
			n.opts.Logger.Warn("phase notice not delivered", "ticket", nt.ticket, "service", nt.service, "err", err)
		}
		return
	}
	nt.wait = min(max(2*nt.wait, n.opts.Retry), n.opts.MaxRetry)
	nt.timer = n.opts.Clock.AfterFunc(nt.wait, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		// After Close, starting would race its Wait; a replaced notice is dropped in attempt.
		if !n.closed {
			n.startLocked(key, nt)
		}
	})
}

func (n *Notifier) current(key string, nt *notice) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.closed && n.pending[key] == nt
}

// worthRetrying is true while the agent is there but cannot take input yet: waiting on
// a prompt, still starting, or too slow to answer this time.
func worthRetrying(err error) bool {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == api.CodeBlocked || apiErr.Code == api.CodeUnavailable
	}
	return errors.Is(err, context.DeadlineExceeded)
}
