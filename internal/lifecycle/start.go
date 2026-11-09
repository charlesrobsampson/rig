// Package lifecycle changes what a ticket has running: starting provisions its
// checkouts; stopping (a later task) tears them down. Everything it does is recorded in
// state.json and announced on the event bus.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
)

type Options struct {
	Dir    string // the tickets directory
	States *state.Manager
	Global config.Global
	Bus    *events.Bus
	Clock  clock.Clock // nil means the real clock
}

type Manager struct {
	opts Options
	reg  *registry.Registry

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(o Options) *Manager {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	return &Manager{opts: o, reg: registry.New(o.Dir, o.States), locks: map[string]*sync.Mutex{}}
}

// lock serializes operations on one ticket, so two starts never provision the same
// checkout twice.
func (m *Manager) lock(id string) (unlock func()) {
	m.mu.Lock()
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func invalid(format string, args ...any) *api.Error {
	return &api.Error{Code: api.CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

// Start provisions one checkout per service: an isolated clone for a service whose
// sandbox is container, a worktree for host. It is idempotent: checkouts that already
// exist are kept as they are, and one left behind by a crash is adopted. If any
// service fails, the checkouts created by this call are removed and nothing is recorded.
func (m *Manager) Start(ctx context.Context, p api.TicketStartParams) (api.TicketStartResult, error) {
	l, unlock, err := m.begin(p.ID)
	if err != nil {
		return api.TicketStartResult{}, err
	}
	defer unlock()
	t, st, ticketDir, statePath := l.ticket, l.state, l.dir, l.statePath
	if st.Phase == domain.PhaseDone {
		return api.TicketStartResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("ticket %q is done", p.ID)}
	}

	var infos []api.CheckoutInfo
	var created []git.Checkout
	rollback := func() {
		for i := len(created) - 1; i >= 0; i-- {
			created[i].Remove(ctx, true) // brand new, so there is nothing to lose
		}
	}
	for _, svc := range t.Services {
		eff := policy.Resolve(m.opts.Global, t, svc, policy.Flags{Sandbox: p.Sandbox})
		spec := git.Spec{Repo: svc.Repo, Branch: svc.Branch, Base: svc.Base, Path: filepath.Join(ticketDir, svc.Path), Kind: kindFor(eff.Sandbox)}
		if err := git.CheckRepo(ctx, svc.Repo); err != nil {
			rollback()
			return api.TicketStartResult{}, invalid("service %s: repository %s: %v", svc.Name, svc.Repo, err)
		}

		info := api.CheckoutInfo{Service: svc.Name, Kind: spec.Kind, Path: spec.Path, Branch: svc.Branch}
		if occupied(spec.Path) {
			if _, err := git.Open(ctx, spec); err != nil {
				rollback()
				return api.TicketStartResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("service %s: %v", svc.Name, err)}
			}
		} else {
			co, err := git.Provision(ctx, spec)
			if err != nil {
				rollback()
				code := api.CodeInvalid
				if errors.Is(err, git.ErrBranchInUse) || errors.Is(err, git.ErrPathInUse) {
					code = api.CodeConflict
				}
				return api.TicketStartResult{}, &api.Error{Code: code, Message: fmt.Sprintf("service %s: %v", svc.Name, err)}
			}
			created = append(created, co)
			info.Created = true
		}
		infos = append(infos, info)
	}

	changed, err := m.record(statePath, st, t, infos)
	if err != nil {
		rollback()
		return api.TicketStartResult{}, err
	}
	for _, name := range changed {
		m.publish(api.EventServiceStatus, p.ID, name, api.ServiceStatusData{Status: domain.ServiceProvisioned})
	}
	if len(changed) > 0 {
		m.publish(api.EventTicketStarted, p.ID, "", api.EmptyData{})
	}

	res, err := m.reg.Get(p.ID)
	if err != nil {
		return api.TicketStartResult{}, err
	}
	return api.TicketStartResult{Summary: res.Summary, Checkouts: infos}, nil
}

func kindFor(s domain.Sandbox) domain.CheckoutKind {
	if s == domain.SandboxHost {
		return domain.CheckoutWorktree
	}
	return domain.CheckoutClone
}

// occupied reports whether path holds something: a non-empty directory (or a file).
// An empty directory is free for a new checkout.
func occupied(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return true
	}
	entries, _ := os.ReadDir(path)
	return len(entries) > 0
}

// record writes the checkouts into state.json and returns the services whose status
// moved to provisioned. Nothing is written when the file already says all of it.
func (m *Manager) record(path string, current state.State, t domain.Ticket, infos []api.CheckoutInfo) ([]string, error) {
	apply := func(s *state.State) (changed []string) {
		if s.Services == nil {
			s.Services = map[string]state.ServiceState{}
		}
		for i, svc := range t.Services {
			info := infos[i]
			before := s.Services[svc.Name]
			after := before
			after.Checkout = state.Checkout{Kind: info.Kind, Path: info.Path}
			after.Branch, after.Base = svc.Branch, svc.Base
			if after.Status == "" || after.Status == domain.ServicePending || after.Status == domain.ServiceStopped || after.Status == domain.ServiceError {
				after.Status = domain.ServiceProvisioned
			}
			if !reflect.DeepEqual(after, before) {
				s.Services[svc.Name] = after
				if before.Status != after.Status {
					changed = append(changed, svc.Name)
				}
			}
		}
		return changed
	}

	probe := current
	probe.Services = copyServices(current.Services)
	apply(&probe)
	if sameServices(probe.Services, current.Services) {
		return nil, nil
	}
	var changed []string
	err := m.opts.States.Update(path, func(s *state.State) error {
		changed = apply(s)
		return nil
	})
	if err != nil {
		return nil, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record checkouts: %v", err)}
	}
	return changed, nil
}

func (m *Manager) publish(typ api.EventType, ticket, service string, data any) {
	if m.opts.Bus != nil {
		m.opts.Bus.Publish(typ, ticket, service, data)
	}
}

func copyServices(in map[string]state.ServiceState) map[string]state.ServiceState {
	out := make(map[string]state.ServiceState, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sameServices(a, b map[string]state.ServiceState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !reflect.DeepEqual(b[k], v) {
			return false
		}
	}
	return true
}
