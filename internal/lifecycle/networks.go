package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/network"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/state"
)

// needsDocker is true when anything in the ticket runs in Docker: a container-sandbox
// agent, a dependency, or a service with a Dockerfile. A ticket that is
// entirely host work must not require Docker to be installed.
func (m *Manager) needsDocker(t domain.Ticket, sandbox domain.Sandbox) bool {
	if len(t.Dependencies) > 0 {
		return true
	}
	for _, svc := range t.Services {
		if policy.Resolve(m.opts.Global, t, svc, policy.Flags{Sandbox: sandbox}).Sandbox == domain.SandboxContainer || svc.Build.Dockerfile != "" {
			return true
		}
	}
	return false
}

func (m *Manager) networkMode(t domain.Ticket, sandbox domain.Sandbox) domain.NetworkMode {
	return policy.Resolve(m.opts.Global, t, t.Services[0], policy.Flags{Sandbox: sandbox}).NetworkMode
}

// provisionNetworks creates the ticket's networks (idempotently) and records them in
// state.json. It returns nil when the ticket needs no Docker.
func (m *Manager) provisionNetworks(ctx context.Context, l loaded, sandbox domain.Sandbox) (*api.NetworkInfo, error) {
	if m.opts.Docker == nil || !m.needsDocker(l.ticket, sandbox) {
		return nil, nil
	}
	nets, err := network.New(m.opts.Docker, m.opts.Clock).Ensure(ctx, l.id, m.networkMode(l.ticket, sandbox))
	if err != nil {
		return nil, dockerError(fmt.Sprintf("ticket %q: prepare its network", l.id), err)
	}
	info := &api.NetworkInfo{Name: nets.Main.Name, ID: nets.Main.ID, Internal: nets.Main.Internal}
	if nets.Egress != nil {
		info.Egress = nets.Egress.Name
	}
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		s.Network = state.Network{Name: nets.Main.Name, ID: nets.Main.ID}
		s.EgressNetwork = nil
		if nets.Egress != nil {
			s.EgressNetwork = &state.Network{Name: nets.Egress.Name, ID: nets.Egress.ID}
		}
		return nil
	})
	if err != nil {
		return nil, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record the network: %v", err)}
	}
	return info, nil
}

// removeNetworks removes the ticket's networks if it has any recorded.
func (m *Manager) removeNetworks(ctx context.Context, id string, st state.State) error {
	if m.opts.Docker == nil || st.Network.Name == "" {
		return nil
	}
	if err := network.New(m.opts.Docker, m.opts.Clock).Remove(ctx, id); err != nil {
		return dockerError(fmt.Sprintf("ticket %q: remove its network", id), err)
	}
	return nil
}

func dockerError(context string, err error) error {
	code := api.CodeInternal
	switch {
	case errors.Is(err, docker.ErrUnavailable):
		code = api.CodeUnavailable
	case errors.Is(err, network.ErrConflict), errors.Is(err, docker.ErrInUse):
		code = api.CodeConflict
	}
	return &api.Error{Code: code, Message: fmt.Sprintf("%s: %v", context, err)}
}
