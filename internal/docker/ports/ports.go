// Package ports gives each service container a host port. A port is recorded in the
// ticket's state.json so it survives restarts, is never shared between tickets, and is
// checked again before it is reused, since something outside rig may have taken it.
package ports

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

// ErrExhausted: every port in the range is taken.
var ErrExhausted = errors.New("no free host port")

// Probe reports whether a host port can be used. The default tries to listen on it;
// a caller that knows a port is held by its own running container says so by
// returning true for it.
type Probe func(port int) bool

type Options struct {
	Dir      string // the tickets directory
	States   *state.Manager
	Min, Max int
	Probe    Probe // nil means try to listen on 127.0.0.1
}

type Allocator struct {
	opts Options
	mu   sync.Mutex
}

func New(o Options) *Allocator {
	if o.Probe == nil {
		o.Probe = ListenProbe
	}
	return &Allocator{opts: o}
}

// ListenProbe is the default Probe: a port is usable if nothing is listening on it.
func ListenProbe(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// Allocate returns the service's host port. The one recorded in state.json is kept if
// no other ticket holds it and it is still usable; otherwise a new one is chosen (the
// lowest free port in the range) and recorded.
func (a *Allocator) Allocate(ctx context.Context, ticket, service string) (int, error) {
	if err := domain.ValidateTicketID(ticket); err != nil {
		return 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	statePath := filepath.Join(a.opts.Dir, ticket, "state.json")
	current, err := a.opts.States.Load(statePath)
	if err != nil {
		return 0, fmt.Errorf("allocate a port for %s/%s: %w", ticket, service, err)
	}
	svc, ok := current.Services[service]
	if !ok {
		return 0, fmt.Errorf("ticket %q has no service %q", ticket, service)
	}
	taken, err := a.heldByOthers(ticket, service)
	if err != nil {
		return 0, err
	}

	if p := svc.HostPort; p != 0 && !taken[p] && a.inRange(p) && a.opts.Probe(p) {
		return p, nil
	}
	for p := a.opts.Min; p <= a.opts.Max; p++ {
		if taken[p] || !a.opts.Probe(p) {
			continue
		}
		if err := a.record(statePath, service, p); err != nil {
			return 0, err
		}
		return p, nil
	}
	return 0, fmt.Errorf("%w in %d-%d (%d held by tickets, the rest in use on this machine)", ErrExhausted, a.opts.Min, a.opts.Max, len(taken))
}

func (a *Allocator) inRange(p int) bool { return p >= a.opts.Min && p <= a.opts.Max }

// Release forgets the service's port so another ticket can use it.
func (a *Allocator) Release(ticket, service string) error {
	if err := domain.ValidateTicketID(ticket); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.record(filepath.Join(a.opts.Dir, ticket, "state.json"), service, 0)
}

func (a *Allocator) record(statePath, service string, port int) error {
	err := a.opts.States.Update(statePath, func(s *state.State) error {
		ss, ok := s.Services[service]
		if !ok {
			return fmt.Errorf("no service %q", service)
		}
		if ss.HostPort == port {
			return errUnchanged
		}
		ss.HostPort = port
		s.Services[service] = ss
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	return err
}

// errUnchanged makes Update skip a write that would change nothing.
var errUnchanged = errors.New("unchanged")

// heldByOthers lists every port recorded for any service other than the one asking.
// A ticket whose state.json cannot be read is skipped: it may be broken, and its
// ports cannot be known.
func (a *Allocator) heldByOthers(ticket, service string) (map[int]bool, error) {
	entries, err := os.ReadDir(a.opts.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[int]bool{}, nil
		}
		return nil, err
	}
	held := map[int]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := a.opts.States.Load(filepath.Join(a.opts.Dir, e.Name(), "state.json"))
		if err != nil {
			continue
		}
		for name, svc := range st.Services {
			if svc.HostPort != 0 && !(e.Name() == ticket && name == service) {
				held[svc.HostPort] = true
			}
		}
	}
	return held, nil
}
