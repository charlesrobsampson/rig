// Package supervisor keeps each service's agent state current. The authority is what
// the agent's own hooks report (agent.event); the terminal backend supplies pane
// facts (is it alive) and a screen guess that only fills gaps. Changes are written to
// state.json and announced as agent.state events (docs/spikes/03-tmux.md §2).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

type Options struct {
	Dir      string // the tickets directory
	States   *state.Manager
	Backend  terminal.Backend
	Bus      *events.Bus
	Clock    clock.Clock // nil means the real clock
	Logger   *slog.Logger
	Interval time.Duration // between polls (default 2s)
	// HookTTL is how long a hook report outranks the screen guess (default 5m).
	HookTTL time.Duration
	// MaxBackoff caps the wait after repeated backend failures (default 30s).
	MaxBackoff time.Duration
}

type Supervisor struct {
	opts Options

	mu    sync.Mutex
	hooks map[key]hookReport
	// offsets is how far into each container agent's events file has been read.
	offsets map[key]int64
}

type key struct{ ticket, service string }

type hookReport struct {
	state domain.AgentState
	at    time.Time
	ready bool
}

func New(o Options) *Supervisor {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Interval == 0 {
		o.Interval = 2 * time.Second
	}
	if o.HookTTL == 0 {
		o.HookTTL = 5 * time.Minute
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 30 * time.Second
	}
	return &Supervisor{opts: o, hooks: map[key]hookReport{}, offsets: map[key]int64{}}
}

func invalid(format string, args ...any) *api.Error {
	return &api.Error{Code: api.CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

// Report handles agent.event: it records what the agent says and applies it at once.
func (s *Supervisor) Report(ctx context.Context, p api.AgentEventParams) error {
	var st domain.AgentState
	ready := false
	switch p.State {
	case "ready":
		st, ready = domain.AgentIdle, true
	case "working", "blocked", "idle", "exited":
		st = domain.AgentState(p.State)
	default:
		return invalid("agent state %q is not one of ready, working, blocked, idle, exited", p.State)
	}
	if err := domain.ValidateTicketID(p.Ticket); err != nil {
		return invalid("%v", err)
	}
	statePath := filepath.Join(s.opts.Dir, p.Ticket, "state.json")
	if _, err := os.Stat(statePath); err != nil {
		return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q not found", p.Ticket)}
	}
	current, err := s.opts.States.Load(statePath)
	if err != nil {
		return err
	}
	svc, ok := current.Services[p.Service]
	if !ok {
		return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no service %q", p.Ticket, p.Service)}
	}

	k := key{p.Ticket, p.Service}
	s.mu.Lock()
	prev := s.hooks[k]
	s.hooks[k] = hookReport{state: st, at: s.opts.Clock.Now(), ready: ready || prev.ready}
	s.mu.Unlock()

	// The pane tells whether the agent can still be talking: a report from a dead pane
	// is a leftover, and a backend that cannot answer must not fail the hook.
	var pane *terminal.Pane
	if svc.Agent.PaneID != "" {
		if got, err := s.opts.Backend.Get(ctx, terminal.PaneID(svc.Agent.PaneID)); err == nil {
			pane = &got
		} else if errors.Is(err, terminal.ErrNotFound) {
			pane = &terminal.Pane{Alive: false}
		}
	}
	return s.apply(statePath, p.Ticket, p.Service, pane)
}

// Ready says whether the service's agent has reported that its interface is up, so
// typed input will not be lost. It forgets on exit or when the daemon restarts.
func (s *Supervisor) Ready(ticket, service string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hooks[key{ticket, service}]
	return ok && h.ready && h.state != domain.AgentExited
}

// merge decides a service's state. A dead pane is exited. A hook report outranks
// everything while fresh; later the screen guess may overrule it. With neither, the
// recorded state stands (it was a hook report from before a daemon restart), except
// that "exited" cannot describe a pane that is alive.
func (s *Supervisor) merge(h *hookReport, pane *terminal.Pane, recorded domain.AgentState) domain.AgentState {
	if pane != nil && !pane.Alive {
		return domain.AgentExited
	}
	guess := domain.AgentUnknown
	if pane != nil {
		guess = pane.Heuristic
	}
	switch {
	case h != nil && s.opts.Clock.Now().Sub(h.at) <= s.opts.HookTTL:
		return h.state
	case guess != domain.AgentUnknown:
		return guess
	case h != nil:
		return h.state
	case recorded == domain.AgentExited:
		return domain.AgentUnknown
	}
	return recorded
}

func (s *Supervisor) apply(statePath, ticket, service string, pane *terminal.Pane) error {
	s.mu.Lock()
	var h *hookReport
	if r, ok := s.hooks[key{ticket, service}]; ok {
		h = &r
	}
	s.mu.Unlock()

	var previous, next domain.AgentState
	err := s.opts.States.Update(statePath, func(st *state.State) error {
		svc, ok := st.Services[service]
		if !ok {
			return errNoChange // the service left the ticket since we looked
		}
		previous = svc.Agent.State
		next = s.merge(h, pane, previous)
		if next == previous {
			return errNoChange
		}
		svc.Agent.State = next
		st.Services[service] = svc
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.opts.Bus.Publish(api.EventAgentState, ticket, service, api.AgentStateData{State: next, Previous: previous})
}

// errNoChange makes Update skip the write when nothing differs.
var errNoChange = errors.New("no change")

// Poll reconciles every launched agent with the backend once. It asks the backend only
// when some agent has a pane to ask about.
func (s *Supervisor) Poll(ctx context.Context) error {
	entries, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type watched struct {
		ticket, service, path, pane string
	}
	var work []watched
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(s.opts.Dir, e.Name(), "state.json")
		st, err := s.opts.States.Load(path)
		if err != nil || st.Phase == domain.PhaseDone {
			continue // an unreadable or finished ticket has nothing to watch
		}
		for name, svc := range st.Services {
			if svc.Agent.PaneID != "" {
				work = append(work, watched{e.Name(), name, path, svc.Agent.PaneID})
			}
		}
	}
	if len(work) == 0 {
		return nil
	}
	for _, w := range work {
		s.readEvents(ctx, w.ticket, w.service)
	}

	panes, err := s.opts.Backend.List(ctx)
	if err != nil {
		return fmt.Errorf("list panes: %w", err)
	}
	byID := make(map[terminal.PaneID]terminal.Pane, len(panes))
	for _, p := range panes {
		byID[p.ID] = p
	}
	for _, w := range work {
		pane, ok := byID[terminal.PaneID(w.pane)]
		if !ok {
			pane = terminal.Pane{ID: terminal.PaneID(w.pane)} // gone: Alive is false
		}
		if err := s.apply(w.path, w.ticket, w.service, &pane); err != nil {
			s.opts.Logger.Warn("update agent state", "ticket", w.ticket, "service", w.service, "err", err)
		}
	}
	return nil
}

// Run polls until ctx is cancelled. After a failed poll it waits longer each time, up to
// MaxBackoff, and returns to the normal interval once a poll succeeds.
func (s *Supervisor) Run(ctx context.Context) {
	delay := s.opts.Interval
	failing := false
	for {
		timer := s.opts.Clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		if err := s.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing {
				s.opts.Logger.Warn("supervisor cannot reach the terminal backend; backing off", "err", err)
			}
			failing = true
			delay = min(max(delay*2, s.opts.Interval), s.opts.MaxBackoff)
			continue
		}
		if failing {
			s.opts.Logger.Info("supervisor reached the terminal backend again")
		}
		failing, delay = false, s.opts.Interval
	}
}

// EventsFile is where a container agent's hooks append their reports, one state per
// line (the agent cannot reach the daemon, so it writes to a mounted directory).
func EventsFile(ticketsDir, ticket, service string) string {
	return filepath.Join(ticketsDir, ticket, ".rig-events", service, "events.log")
}

// readEvents applies the new lines of a service's events file as hook reports. The
// first time a file is seen only its last line counts: older lines are history that a
// restarted daemon must not replay as a string of state changes.
func (s *Supervisor) readEvents(ctx context.Context, ticket, service string) {
	path := EventsFile(s.opts.Dir, ticket, service)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	k := key{ticket, service}
	s.mu.Lock()
	offset, seen := s.offsets[k]
	s.mu.Unlock()
	if seen && int64(len(data)) < offset {
		offset = 0 // the file was truncated: a new agent run
	}

	end := strings.LastIndexByte(string(data), '\n') + 1 // only complete lines
	if end == 0 || int64(end) <= offset && seen {
		return
	}
	chunk := strings.Split(strings.TrimSuffix(string(data[offset:end]), "\n"), "\n")
	if !seen {
		chunk = chunk[len(chunk)-1:]
	}
	s.mu.Lock()
	s.offsets[k] = int64(end)
	s.mu.Unlock()
	for _, line := range chunk {
		if line = strings.TrimSpace(line); line != "" {
			if err := s.Report(ctx, api.AgentEventParams{Ticket: ticket, Service: service, State: line}); err != nil {
				s.opts.Logger.Debug("ignored an agent event", "ticket", ticket, "service", service, "line", line, "err", err)
			}
		}
	}
}
