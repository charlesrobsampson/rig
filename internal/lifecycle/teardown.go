package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/network"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

// teardown stops everything that runs for the ticket, in dependency order: agents,
// services, dependencies, the proxy, any other container carrying the ticket's label,
// then the networks (skipped, with a note, if a container could not be removed, since
// it would still be attached). It tries every step and returns what it removed and
// what failed.
func (m *Manager) teardown(ctx context.Context, id string, l loaded, force bool) (removed []string, failures []error) {
	before := m.ticketObjects(ctx, id)

	step := func(err error) {
		if err == nil {
			return
		}
		var joined interface{ Unwrap() []error }
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			joined = j
			failures = append(failures, joined.Unwrap()...)
			return
		}
		failures = append(failures, err)
	}
	step(m.closeAgents(ctx, id, l, force))
	step(m.removeServiceContainers(ctx, id, l.ticket, l.state))
	step(m.removeDependencyContainers(ctx, id, l.ticket))
	step(m.removeProxy(ctx, id))
	step(m.sweepContainers(ctx, id))

	if len(failures) == 0 {
		step(m.removeNetworks(ctx, id, l.state))
		step(m.sweepNetworks(ctx, id))
	} else {
		failures = append(failures, errors.New("networks were left in place because containers could not be removed"))
	}

	after := m.ticketObjects(ctx, id)
	for name := range before {
		if !after[name] {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	return removed, failures
}

// ticketObjects names the Docker containers and networks that carry the ticket's label.
func (m *Manager) ticketObjects(ctx context.Context, id string) map[string]bool {
	out := map[string]bool{}
	if m.opts.Docker == nil {
		return out
	}
	labels := map[string]string{docker.LabelTicket: id}
	if list, err := m.opts.Docker.ListContainers(ctx, labels); err == nil {
		for _, c := range list {
			out["container "+c.Name] = true
		}
	}
	if list, err := m.opts.Docker.ListNetworks(ctx, labels); err == nil {
		for _, n := range list {
			out["network "+n.Name] = true
		}
	}
	return out
}

// sweepContainers removes containers still labelled for the ticket that the ordered
// steps did not know about (left by a crash, or created by an older rig).
func (m *Manager) sweepContainers(ctx context.Context, id string) error {
	if m.opts.Docker == nil {
		return nil
	}
	list, err := m.opts.Docker.ListContainers(ctx, map[string]string{docker.LabelTicket: id})
	if err != nil {
		return dockerError(fmt.Sprintf("ticket %q: list its containers", id), err)
	}
	var failures []error
	for _, c := range list {
		if err := m.opts.Docker.RemoveContainer(ctx, c.ID, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
			failures = append(failures, dockerError(fmt.Sprintf("remove leftover container %s", c.Name), err))
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) sweepNetworks(ctx context.Context, id string) error {
	if m.opts.Docker == nil {
		return nil
	}
	list, err := m.opts.Docker.ListNetworks(ctx, map[string]string{docker.LabelTicket: id})
	if err != nil {
		return dockerError(fmt.Sprintf("ticket %q: list its networks", id), err)
	}
	var failures []error
	for _, n := range list {
		if err := m.opts.Docker.RemoveNetwork(ctx, n.ID); err != nil && !errors.Is(err, docker.ErrNotFound) {
			failures = append(failures, dockerError(fmt.Sprintf("remove leftover network %s", n.Name), err))
		}
	}
	return errors.Join(failures...)
}

// syncTornDown makes state.json say what is actually gone: a service or dependency
// whose container no longer exists forgets its container id and host port, and the
// networks are forgotten once they are removed. It runs after a failed teardown too, so
// a later retry starts from the truth.
func (m *Manager) syncTornDown(ctx context.Context, l loaded) {
	if m.opts.Docker == nil {
		return
	}
	remaining := m.ticketObjects(ctx, l.id)
	m.opts.States.Update(l.statePath, func(s *state.State) error {
		for _, svc := range l.ticket.Services {
			if !remaining["container "+ContainerName(l.id, svc.Name)] {
				ss := s.Services[svc.Name]
				ss.ContainerID = ""
				if runsInDocker(svc) {
					ss.HostPort = 0
				}
				if !remaining["container "+AgentContainerName(l.id, svc.Name)] && ss.Agent.PaneID == "" {
					ss.Agent.ContainerID = ""
				}
				s.Services[svc.Name] = ss
			}
		}
		for _, dep := range l.ticket.Dependencies {
			if !remaining["container "+DependencyName(l.id, dep.Name)] {
				s.Dependencies[dep.Name] = state.DependencyState{Status: domain.ServiceStopped}
			}
		}
		if !remaining["network "+network.Name(l.id)] {
			s.Network = state.Network{}
		}
		if !remaining["network "+network.EgressName(l.id)] {
			s.EgressNetwork = nil
		}
		return nil
	})
}

// partialStop is the error for a teardown that did not finish: what failed, and what
// is still there.
func (m *Manager) partialStop(ctx context.Context, id string, failures []error) error {
	var remaining []string
	for name := range m.ticketObjects(ctx, id) {
		remaining = append(remaining, name)
	}
	sort.Strings(remaining)
	details := make([]string, 0, len(failures)+2)
	code := api.CodeInternal
	for _, f := range failures {
		details = append(details, f.Error())
		var apiErr *api.Error
		if code == api.CodeInternal && errors.As(f, &apiErr) && apiErr.Code != api.CodeInternal {
			code = apiErr.Code // the first specific cause names the whole failure
		}
	}
	if len(remaining) > 0 {
		details = append(details, "still there: "+strings.Join(remaining, ", "))
	}
	details = append(details, "the checkouts were kept; run stop again once the problem is fixed")
	return &api.Error{Code: code, Message: fmt.Sprintf("stopping ticket %q did not finish: %d step(s) failed", id, len(failures)), Details: details}
}

// alreadyStopped is true for a ticket that was started and then stopped: every service
// says stopped and nothing of it remains in Docker. A ticket that never ran is not
// "already stopped"; stopping it is an error (there is nothing to stop).
func (m *Manager) alreadyStopped(ctx context.Context, id string, t domain.Ticket, st state.State) bool {
	if len(t.Services) == 0 {
		return false
	}
	for _, svc := range t.Services {
		if st.Services[svc.Name].Status != domain.ServiceStopped {
			return false
		}
	}
	return len(m.ticketObjects(ctx, id)) == 0
}
