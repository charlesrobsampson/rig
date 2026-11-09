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
// A container-sandbox service's agent runs in its own container (launchContainerAgent);
// without Docker it is not started on the host instead, since that would quietly drop
// the isolation the sandbox promises. A host-sandbox service's agent runs on the host,
// which start warns about.
func (m *Manager) launchAgents(ctx context.Context, l loaded, checkouts []api.CheckoutInfo, sandbox domain.Sandbox, net *api.NetworkInfo) (agents []api.AgentInfo, warnings []string) {
	if m.opts.Backend == nil {
		return nil, nil
	}
	var out []api.AgentInfo
	ready := false
	for i, svc := range l.ticket.Services {
		info := api.AgentInfo{Service: svc.Name}
		eff := policy.Resolve(m.opts.Global, l.ticket, svc, policy.Flags{Sandbox: sandbox})
		if eff.Sandbox != domain.SandboxHost && (m.opts.Docker == nil || net == nil) {
			info.Note = "no agent started: a container-sandbox agent needs Docker; start with --sandbox host to run it on the host"
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
		var al agentLaunch
		var err error
		if eff.Sandbox == domain.SandboxHost {
			al, err = m.prepareHostAgent(l, svc, eff, checkouts[i].Path)
			if err == nil {
				warnings = append(warnings, fmt.Sprintf("service %s: its agent runs directly on this machine (sandbox: host), with your user's access to everything you can reach; use the default container sandbox to isolate it", svc.Name))
			}
		} else {
			al, err = m.prepareContainerAgent(ctx, l, svc, eff, checkouts[i], net)
		}
		if err != nil {
			info.Error = err.Error()
			out = append(out, info)
			continue
		}
		id, err := m.spawnAgent(ctx, l, svc, al)
		if err != nil {
			info.Error = err.Error()
		} else {
			info.PaneID, info.Launched, info.Container = string(id), true, al.containerName
		}
		out = append(out, info)
	}
	return out, warnings
}

// agentLaunch is what the terminal backend is asked to run, and where the agent lives.
type agentLaunch struct {
	command []string
	env     map[string]string
	dir     string // the pane's working directory on the host
	// containerName and containerID are set when the agent runs in a container.
	containerName, containerID string
}

// agentRequest is the harness request both kinds of launch start from: the secrets and
// auth profile resolved, nothing about where the agent runs.
func (m *Manager) agentRequest(l loaded, svc domain.Service, eff policy.Effective) (harness.Harness, harness.Request, error) {
	h, err := harness.Get(eff.Harness)
	if err != nil {
		return nil, harness.Request{}, err
	}
	req := harness.Request{Ticket: l.ticket, Service: svc.Name, Phase: l.state.Phase, TicketDir: l.dir, Network: eff.NetworkMode}
	names := append([]string(nil), l.ticket.Secrets...)
	if eff.Auth != "" {
		profile, ok := m.opts.Global.Auth.Profiles[eff.Auth]
		if !ok {
			return nil, req, fmt.Errorf("auth profile %q is not defined", eff.Auth)
		}
		if profile.Type == domain.AuthHostLogin && eff.Sandbox == domain.SandboxContainer {
			return nil, req, fmt.Errorf("auth profile %q uses the machine's own login, which a container cannot have; use a token, API key or cloud profile, or sandbox: host", eff.Auth)
		}
		req.Profile = &profile
		names = append(names, profile.Secrets...)
	}
	if len(names) > 0 {
		if m.opts.Secrets == nil {
			return nil, req, fmt.Errorf("secrets %s cannot be resolved yet", strings.Join(names, ", "))
		}
		if req.Secrets, err = m.opts.Secrets(names); err != nil {
			return nil, req, fmt.Errorf("resolve secrets: %w", err)
		}
	}
	return h, req, nil
}

func (m *Manager) prepareHostAgent(l loaded, svc domain.Service, eff policy.Effective, dir string) (agentLaunch, error) {
	h, req, err := m.agentRequest(l, svc, eff)
	if err != nil {
		return agentLaunch{}, err
	}
	req.Dir, req.HookCommand = dir, shellQuote(m.rigBinary())+" agent-event"
	launch, err := h.Command(req)
	if err != nil {
		return agentLaunch{}, err
	}
	for k, v := range m.opts.AgentEnv {
		if _, taken := launch.Env[k]; !taken {
			launch.Env[k] = v
		}
	}
	return agentLaunch{command: launch.Command, env: launch.Env, dir: launch.Dir}, nil
}

// spawnAgent opens the pane and records it in state.json.
func (m *Manager) spawnAgent(ctx context.Context, l loaded, svc domain.Service, al agentLaunch) (terminal.PaneID, error) {
	id, err := m.opts.Backend.Spawn(ctx, terminal.SpawnSpec{Ticket: l.id, Service: svc.Name, Dir: al.dir, Command: al.command, Env: al.env})
	if err != nil {
		return "", fmt.Errorf("start the agent: %w", err)
	}
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		ss := s.Services[svc.Name]
		ss.Agent = state.Agent{Backend: m.opts.BackendName, PaneID: string(id), State: domain.AgentUnknown, ContainerID: al.containerID}
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
		return m.removeAgentContainers(ctx, id, l.ticket, force)
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
	if err := m.removeAgentContainers(ctx, id, l.ticket, force); err != nil {
		return err
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
