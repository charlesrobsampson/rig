// Package network creates and removes a ticket's Docker networks (docs/formats.md §7):
// `rig-<ticket>` for everything in the ticket, created with Docker's internal flag in
// `internal` mode (no route out), plus `rig-<ticket>-egress`, a normal bridge that only
// the egress proxy joins (docs/spikes/05-egress.md).
package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// ErrConflict: a network of that name exists but is not what this ticket needs (wrong
// isolation, or not made by rig for this ticket).
var ErrConflict = errors.New("network conflict")

func Name(ticket string) string       { return "rig-" + ticket }
func EgressName(ticket string) string { return "rig-" + ticket + "-egress" }

// Networks are what a ticket has. Egress is nil unless the mode is internal.
type Networks struct {
	Main   docker.NetworkInfo
	Egress *docker.NetworkInfo
}

type Manager struct {
	d     docker.Docker
	clock clock.Clock
	// DetachTimeout is how long Remove waits for containers to leave (default 15s).
	DetachTimeout time.Duration
	// Poll is how often it looks again (default 250ms).
	Poll time.Duration
}

func New(d docker.Docker, c clock.Clock) *Manager {
	if c == nil {
		c = clock.Real()
	}
	return &Manager{d: d, clock: c, DetachTimeout: 15 * time.Second, Poll: 250 * time.Millisecond}
}

// Ensure makes the ticket's networks exist for the mode and returns them. It is safe to
// repeat: a network that is already there is reused. One that exists with the wrong
// isolation (an `internal` flag that does not match the mode) is an error, never
// silently kept, because a sandbox that is not isolated is the one failure that matters.
func (m *Manager) Ensure(ctx context.Context, ticket string, mode domain.NetworkMode) (Networks, error) {
	internal := mode == domain.NetworkInternal
	main, err := m.ensureOne(ctx, docker.NetworkSpec{Name: Name(ticket), Internal: internal, Labels: docker.Labels(ticket, "", docker.RoleNetwork)})
	if err != nil {
		return Networks{}, err
	}
	nets := Networks{Main: main}
	if internal {
		egress, err := m.ensureOne(ctx, docker.NetworkSpec{Name: EgressName(ticket), Labels: docker.Labels(ticket, "", docker.RoleNetwork)})
		if err != nil {
			return Networks{}, err
		}
		nets.Egress = &egress
	}
	return nets, nil
}

func (m *Manager) ensureOne(ctx context.Context, spec docker.NetworkSpec) (docker.NetworkInfo, error) {
	info, err := m.d.InspectNetwork(ctx, spec.Name)
	if errors.Is(err, docker.ErrNotFound) {
		info, err = m.d.CreateNetwork(ctx, spec)
		if errors.Is(err, docker.ErrExists) { // another start created it between the two calls
			info, err = m.d.InspectNetwork(ctx, spec.Name)
		}
	}
	if err != nil {
		return docker.NetworkInfo{}, fmt.Errorf("network %s: %w", spec.Name, err)
	}
	if info.Internal != spec.Internal {
		return docker.NetworkInfo{}, fmt.Errorf("%w: network %s exists with internal=%v but this ticket needs internal=%v; stop the ticket (or remove the network) and start again", ErrConflict, spec.Name, info.Internal, spec.Internal)
	}
	if info.Labels[docker.LabelTicket] != spec.Labels[docker.LabelTicket] {
		return docker.NetworkInfo{}, fmt.Errorf("%w: network %s exists but was not created by rig for ticket %q; rig will not use or remove it", ErrConflict, spec.Name, spec.Labels[docker.LabelTicket])
	}
	return info, nil
}

// Remove deletes the ticket's networks (the egress one first). Containers still
// attached are given DetachTimeout to leave, then the error names them. Removing what
// is already gone is fine.
func (m *Manager) Remove(ctx context.Context, ticket string) error {
	for _, name := range []string{EgressName(ticket), Name(ticket)} {
		if err := m.removeOne(ctx, name, ticket); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) removeOne(ctx context.Context, name, ticket string) error {
	deadline := m.clock.After(m.DetachTimeout)
	for {
		info, err := m.d.InspectNetwork(ctx, name)
		if errors.Is(err, docker.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("network %s: %w", name, err)
		}
		if info.Labels[docker.LabelTicket] != ticket {
			return fmt.Errorf("%w: network %s was not created by rig for ticket %q; rig will not remove it", ErrConflict, name, ticket)
		}
		err = m.d.RemoveNetwork(ctx, info.ID)
		if err == nil || errors.Is(err, docker.ErrNotFound) {
			return nil
		}
		if !errors.Is(err, docker.ErrInUse) {
			return fmt.Errorf("network %s: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("network %s is still in use by %s: %w", name, m.attached(ctx, info.ID), err)
		case <-m.clock.After(m.Poll):
		}
	}
}

func (m *Manager) attached(ctx context.Context, id string) string {
	info, err := m.d.InspectNetwork(ctx, id)
	if err != nil || len(info.Containers) == 0 {
		return "containers that have not detached yet"
	}
	names := make([]string, 0, len(info.Containers))
	for _, c := range info.Containers {
		if ci, err := m.d.InspectContainer(ctx, c); err == nil {
			names = append(names, ci.Name)
		} else {
			names = append(names, c)
		}
	}
	return strings.Join(names, ", ")
}
