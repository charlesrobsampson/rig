package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

func DependencyName(ticket, name string) string { return "rig-" + ticket + "-dep-" + name }

// startDependencies runs the ticket's sidecars (a database, a queue) on its network
// before any service, and waits for each to be up. A dependency that cannot start
// aborts the start: the dependencies this call created are removed and the error says
// why, with the container's last log lines.
func (m *Manager) startDependencies(ctx context.Context, l loaded, net *api.NetworkInfo) ([]api.ServiceContainerInfo, error) {
	if m.opts.Docker == nil || net == nil || len(l.ticket.Dependencies) == 0 {
		return nil, nil
	}
	var out []api.ServiceContainerInfo
	type madeDep struct{ name, id string }
	var created []madeDep
	cleanup := func() {
		for _, c := range created {
			m.opts.Docker.RemoveContainer(context.WithoutCancel(ctx), c.id, true)
			m.recordDependency(l, c.name, "", domain.ServiceStopped)
		}
	}
	for _, dep := range l.ticket.Dependencies {
		name := DependencyName(l.id, dep.Name)
		info := api.ServiceContainerInfo{Service: dep.Name, Name: name}

		if existing, err := m.opts.Docker.InspectContainer(ctx, name); err == nil {
			if existing.State == docker.StateRunning {
				info.ID, info.Running, info.Note = existing.ID, true, "already running"
				m.recordDependency(l, dep.Name, existing.ID, domain.ServiceRunning)
				out = append(out, info)
				continue
			}
			if err := m.opts.Docker.RemoveContainer(ctx, name, true); err != nil {
				cleanup()
				return nil, dockerError(fmt.Sprintf("dependency %s: replace the stopped container", dep.Name), err)
			}
		}

		// A dependency sees only its own env: the ticket's env and secrets are for the
		// services, and nothing of the dependency's is passed on to them.
		c, err := m.opts.Docker.CreateContainer(ctx, docker.ContainerSpec{
			Name: name, Image: dep.Image, Env: dep.Env, Labels: docker.Labels(l.id, dep.Name, docker.RoleDependency),
			Network: net.Name, Aliases: []string{dep.Name},
		})
		if err != nil {
			cleanup()
			return nil, dockerError(fmt.Sprintf("dependency %s (%s): create", dep.Name, dep.Image), err)
		}
		created = append(created, madeDep{dep.Name, c.ID})
		if err := m.opts.Docker.StartContainer(ctx, c.ID); err != nil {
			cleanup()
			return nil, dockerError(fmt.Sprintf("dependency %s: start", dep.Name), err)
		}
		if err := m.waitDependencyUp(ctx, c.ID); err != nil {
			msg := fmt.Sprintf("dependency %s: %v", dep.Name, err)
			if tail := m.logTail(ctx, name); tail != "" {
				msg += "\nlast log lines:\n" + tail
			}
			m.recordDependency(l, dep.Name, "", domain.ServiceError)
			cleanup()
			return nil, &api.Error{Code: api.CodeUnavailable, Message: msg}
		}
		m.recordDependency(l, dep.Name, c.ID, domain.ServiceRunning)
		info.ID, info.Running = c.ID, true
		out = append(out, info)
	}
	return out, nil
}

// waitDependencyUp returns once the container has stayed up for the settle time and,
// if its image defines a healthcheck, reports healthy. It fails when the container
// exits or turns unhealthy, or the startup timeout passes.
func (m *Manager) waitDependencyUp(ctx context.Context, id string) error {
	timeout, poll, settle := m.opts.ServiceStartTimeout, m.opts.HealthPoll, m.opts.Settle
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	if poll == 0 {
		poll = 500 * time.Millisecond
	}
	if settle == 0 {
		settle = 2 * time.Second
	}
	deadline := m.opts.Clock.After(timeout)
	settled := m.opts.Clock.After(settle)
	isSettled := false
	if settle < 0 { // a caller that cannot wait (a fake clock) asks to check once
		isSettled, settled = true, nil
	}
	for {
		c, err := m.opts.Docker.InspectContainer(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect: %w", err)
		}
		switch {
		case c.State != docker.StateRunning:
			return fmt.Errorf("the container exited with code %d", c.ExitCode)
		case c.Health == docker.HealthUnhealthy:
			return errors.New("the container reports unhealthy")
		case isSettled && (c.Health == docker.HealthNone || c.Health == docker.HealthHealthy):
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("not healthy after %s", timeout)
		case <-settled:
			isSettled, settled = true, nil
		case <-m.opts.Clock.After(poll):
		}
	}
}

func (m *Manager) recordDependency(l loaded, name, containerID string, status domain.ServiceStatus) {
	m.opts.States.Update(l.statePath, func(s *state.State) error {
		if s.Dependencies == nil {
			s.Dependencies = map[string]state.DependencyState{}
		}
		s.Dependencies[name] = state.DependencyState{ContainerID: containerID, Status: status}
		return nil
	})
}

// removeDependencyContainers removes the ticket's sidecars; already-gone ones are fine.
func (m *Manager) removeDependencyContainers(ctx context.Context, id string, t domain.Ticket) error {
	if m.opts.Docker == nil {
		return nil
	}
	grace := m.opts.StopGrace
	if grace == 0 {
		grace = 10 * time.Second
	}
	for _, dep := range t.Dependencies {
		name := DependencyName(id, dep.Name)
		if err := m.opts.Docker.StopContainer(ctx, name, grace); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return dockerError(fmt.Sprintf("ticket %q: stop dependency %s", id, dep.Name), err)
		}
		if err := m.opts.Docker.RemoveContainer(ctx, name, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return dockerError(fmt.Sprintf("ticket %q: remove dependency %s", id, dep.Name), err)
		}
	}
	return nil
}
