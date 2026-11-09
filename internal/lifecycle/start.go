// Package lifecycle changes what a ticket has running: starting provisions its
// checkouts; stopping (a later task) tears them down. Everything it does is recorded in
// state.json and announced on the event bus.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"github.com/charlesrobsampson/rig/internal/contract"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/agentimage"
	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/ports"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/egress"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

type Options struct {
	Dir    string // the tickets directory
	States *state.Manager
	Global config.Global
	Bus    *events.Bus
	Clock  clock.Clock // nil means the real clock

	// Backend runs agents. Without one, starting only provisions checkouts.
	Backend     terminal.Backend
	BackendName string // recorded in state.json as agent.backend
	// RigBinary is what agent hooks run to report state (default "rig"); AgentEnv is
	// added to every agent's environment so the hooks find this daemon.
	RigBinary string
	AgentEnv  map[string]string
	// Secrets resolves secret names to values for agent launches (task 41). Without
	// it, a launch that needs a secret fails with an explanation.
	Secrets func(names []string) (map[string]string, error)
	// Docker runs the ticket's networks and containers. Without it, starting only
	// provisions checkouts (and agents on the host).
	Docker docker.Docker
	// HealthProbe says whether a URL answers (a service's healthcheck); nil means an
	// HTTP GET that accepts any status below 400.
	HealthProbe func(ctx context.Context, url string) bool
	// ServiceStartTimeout is how long a service has to become healthy (default 60s);
	// HealthPoll is how often it is checked (default 500ms); Settle is how long a
	// service without a healthcheck must stay up to count as started (default 2s; a
	// negative value checks once and does not wait).
	ServiceStartTimeout time.Duration
	HealthPoll          time.Duration
	Settle              time.Duration
	// GH runs the GitHub CLI for pull-request pushes (nil means the real `gh`).
	GH git.GH
	// EgressBinary finds the egress proxy's binary (nil means the embedded one).
	EgressBinary egress.BinaryFinder
	// StopGrace is how long a stopping container gets before it is killed (default 10s).
	StopGrace time.Duration
	// OnContractResolved is called after a human accepts or rejects a proposal (the
	// daemon uses it to tell the agents); reason is the rejection reason.
	OnContractResolved func(ticket string, p contract.Proposal, outcome, reason string)
}

type Manager struct {
	opts         Options
	reg          *registry.Registry
	ports        *ports.Allocator
	agentImages  *agentimage.Builder
	egressImages *egress.Images

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// capMu guards starting: the tickets whose start has been let through the limit but
	// is not yet recorded in state.json, so two starts at once cannot both take the last slot.
	capMu    sync.Mutex
	starting map[string]bool
}

func New(o Options) *Manager {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	m := &Manager{opts: o, reg: registry.New(o.Dir, o.States), locks: map[string]*sync.Mutex{}, starting: map[string]bool{}}
	if o.Docker != nil {
		min, max := o.Global.Docker.PortRange.Min, o.Global.Docker.PortRange.Max
		if min == 0 && max == 0 { // a Global built without the defaults
			min, max = 41000, 41999
		}
		m.egressImages = egress.NewImages(o.Docker, o.EgressBinary, "")
		m.agentImages = agentimage.New(o.Docker, o.Global.Docker.ClaudeVersion, o.Global.Docker.ExtraCACerts)
		m.ports = ports.New(ports.Options{Dir: o.Dir, States: o.States, Min: min, Max: max, Probe: m.portUsable})
	}
	return m
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

// Busy reports whether an operation on the ticket is under way (a start or a stop part
// way through creating or removing things). Anything that compares the ticket with what
// exists must leave a busy ticket alone: what it sees is not yet, or no longer, recorded.
func (m *Manager) Busy(id string) bool {
	m.mu.Lock()
	l, ok := m.locks[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	if l.TryLock() {
		l.Unlock()
		return false
	}
	return true
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

	if p.DryRun {
		launches, err := m.previewLaunches(t, st, ticketDir, p.Sandbox)
		if err != nil {
			return api.TicketStartResult{}, err
		}
		res, err := m.reg.Get(p.ID)
		if err != nil {
			return api.TicketStartResult{}, err
		}
		return api.TicketStartResult{Summary: res.Summary, Launches: launches}, nil
	}

	release, err := m.reserveSlot(p.ID, st)
	if err != nil {
		return api.TicketStartResult{}, err
	}
	defer release()

	if err := m.checkSecrets(l, p.Sandbox); err != nil {
		return api.TicketStartResult{}, err
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

	if err := m.validateServices(l, infos); err != nil {
		return api.TicketStartResult{}, err
	}
	netInfo, err := m.provisionNetworks(ctx, l, p.Sandbox)
	if err != nil {
		return api.TicketStartResult{}, err
	}
	proxyEnv, err := m.startProxy(ctx, l, netInfo)
	if err != nil {
		return api.TicketStartResult{}, err
	}
	deps, err := m.startDependencies(ctx, l, netInfo)
	if err != nil {
		return api.TicketStartResult{}, err
	}
	containers := m.startServices(ctx, l, infos, netInfo, proxyEnv)
	agents, warnings := m.launchAgents(ctx, l, infos, p.Sandbox, netInfo, proxyEnv)

	res, err := m.reg.Get(p.ID)
	if err != nil {
		return api.TicketStartResult{}, err
	}
	return api.TicketStartResult{Summary: res.Summary, Checkouts: infos, Agents: agents, Network: netInfo, Containers: containers, Dependencies: deps, Warnings: warnings}, nil
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
