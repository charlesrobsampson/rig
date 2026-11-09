package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/harness"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

// launchAgents starts one agent pane per service that can have one and records the
// pane in state.json. A service whose agent cannot start is reported in the result;
// the checkouts stay, and starting again retries (it is idempotent).
//
// Only host-sandbox services get an agent for now: an agent for a container-sandbox
// service must run in a container (Docker support), and running it on the host instead
// would quietly drop the isolation that sandbox promises.
func (m *Manager) launchAgents(ctx context.Context, l loaded, checkouts []api.CheckoutInfo, sandbox domain.Sandbox) []api.AgentInfo {
	if m.opts.Backend == nil {
		return nil
	}
	var out []api.AgentInfo
	ready := false
	for i, svc := range l.ticket.Services {
		info := api.AgentInfo{Service: svc.Name}
		eff := policy.Resolve(m.opts.Global, l.ticket, svc, policy.Flags{Sandbox: sandbox})
		if eff.Sandbox != domain.SandboxHost {
			info.Note = "no agent started: a container-sandbox agent needs Docker support; start with --sandbox host to run it on the host"
			out = append(out, info)
			continue
		}

		if pane := l.state.Services[svc.Name].Agent.PaneID; pane != "" {
			p, err := m.opts.Backend.Get(ctx, terminal.PaneID(pane))
			if err == nil && p.Alive {
				info.PaneID, info.Note = pane, "already running"
				out = append(out, info)
				continue
			}
			if err == nil {
				// a finished pane some backends keep would block the new one's name
				m.opts.Backend.Close(ctx, terminal.PaneID(pane))
			}
		}

		if !ready {
			if err := m.opts.Backend.Ready(ctx); err != nil {
				info.Error = err.Error()
				out = append(out, info)
				continue
			}
			ready = true
		}
		id, err := m.launchAgent(ctx, l, svc, eff, checkouts[i].Path)
		if err != nil {
			info.Error = err.Error()
		} else {
			info.PaneID, info.Launched = string(id), true
		}
		out = append(out, info)
	}
	return out
}

func (m *Manager) launchAgent(ctx context.Context, l loaded, svc domain.Service, eff policy.Effective, dir string) (terminal.PaneID, error) {
	h, err := harness.Get(eff.Harness)
	if err != nil {
		return "", err
	}
	req := harness.Request{
		Ticket: l.ticket, Service: svc.Name, Phase: l.state.Phase, TicketDir: l.dir,
		Dir: dir, Network: eff.NetworkMode, HookCommand: shellQuote(m.rigBinary()) + " agent-event",
	}
	names := append([]string(nil), l.ticket.Secrets...)
	if eff.Auth != "" {
		profile, ok := m.opts.Global.Auth.Profiles[eff.Auth]
		if !ok {
			return "", fmt.Errorf("auth profile %q is not defined", eff.Auth)
		}
		req.Profile = &profile
		names = append(names, profile.Secrets...)
	}
	if len(names) > 0 {
		if m.opts.Secrets == nil {
			return "", fmt.Errorf("secrets %s cannot be resolved yet", strings.Join(names, ", "))
		}
		if req.Secrets, err = m.opts.Secrets(names); err != nil {
			return "", fmt.Errorf("resolve secrets: %w", err)
		}
	}
	launch, err := h.Command(req)
	if err != nil {
		return "", err
	}
	for k, v := range m.opts.AgentEnv {
		if _, taken := launch.Env[k]; !taken {
			launch.Env[k] = v
		}
	}

	id, err := m.opts.Backend.Spawn(ctx, terminal.SpawnSpec{Ticket: l.id, Service: svc.Name, Dir: launch.Dir, Command: launch.Command, Env: launch.Env})
	if err != nil {
		return "", fmt.Errorf("start the agent: %w", err)
	}
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		ss := s.Services[svc.Name]
		ss.Agent = state.Agent{Backend: m.opts.BackendName, PaneID: string(id), State: domain.AgentUnknown}
		s.Services[svc.Name] = ss
		return nil
	})
	if err != nil {
		m.opts.Backend.Close(ctx, id) // an agent rig cannot find again must not keep running
		return "", fmt.Errorf("record the agent: %w", err)
	}
	return id, nil
}

func (m *Manager) rigBinary() string {
	if m.opts.RigBinary != "" {
		return m.opts.RigBinary
	}
	return "rig"
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// closeAgents closes every service's agent pane and records the agents as exited. A
// pane that is already gone is fine; a backend that cannot be reached is an error
// unless force says to carry on anyway (a checkout must not be removed under an agent
// that may still be running).
func (m *Manager) closeAgents(ctx context.Context, id string, l loaded, force bool) error {
	if m.opts.Backend == nil {
		return nil
	}
	var closed []string
	for _, svc := range l.ticket.Services {
		pane := l.state.Services[svc.Name].Agent.PaneID
		if pane == "" {
			continue
		}
		err := m.opts.Backend.Close(ctx, terminal.PaneID(pane))
		switch {
		case err == nil || errors.Is(err, terminal.ErrNotFound):
			closed = append(closed, svc.Name)
		case force:
			closed = append(closed, svc.Name)
		default:
			return &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("cannot close the agent of service %s: %v (stop with force to carry on anyway)", svc.Name, err)}
		}
	}
	if len(closed) == 0 {
		return nil
	}
	previous := map[string]domain.AgentState{}
	err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		for _, name := range closed {
			ss := s.Services[name]
			previous[name] = ss.Agent.State
			ss.Agent.PaneID, ss.Agent.State = "", domain.AgentExited
			s.Services[name] = ss
		}
		return nil
	})
	if err != nil {
		return &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record closed agents: %v", err)}
	}
	for _, name := range closed {
		if previous[name] != domain.AgentExited {
			m.publish(api.EventAgentState, id, name, api.AgentStateData{State: domain.AgentExited, Previous: previous[name]})
		}
	}
	return nil
}

// Attach returns the command that shows a service's agent pane.
func (m *Manager) Attach(ctx context.Context, p api.AgentAttachParams) (api.AgentAttachResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.AgentAttachResult{}, err
	}
	defer unlock()

	service := p.Service
	if service == "" {
		if len(l.ticket.Services) != 1 {
			names := make([]string, len(l.ticket.Services))
			for i, s := range l.ticket.Services {
				names[i] = s.Name
			}
			return api.AgentAttachResult{}, invalid("ticket %q has several services; say which one (%s)", p.Ticket, strings.Join(names, ", "))
		}
		service = l.ticket.Services[0].Name
	}
	ss, ok := l.state.Services[service]
	if !ok {
		return api.AgentAttachResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no service %q", p.Ticket, service)}
	}
	if ss.Agent.PaneID == "" || m.opts.Backend == nil {
		return api.AgentAttachResult{}, &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("service %q has no agent running; start the ticket first", service)}
	}
	cmd, err := m.opts.Backend.Attach(ctx, terminal.PaneID(ss.Agent.PaneID))
	switch {
	case errors.Is(err, terminal.ErrNotFound):
		return api.AgentAttachResult{}, &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("the agent of service %q is no longer running; start the ticket again", service)}
	case errors.Is(err, terminal.ErrUnavailable):
		return api.AgentAttachResult{}, &api.Error{Code: api.CodeUnavailable, Message: err.Error()}
	case err != nil:
		return api.AgentAttachResult{}, err
	}
	return api.AgentAttachResult{Service: service, Command: cmd}, nil
}
