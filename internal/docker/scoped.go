package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

// LabelHome names the rig home (its directory) whose daemon made an object. One Docker
// can be shared by several homes, each with its own daemon, so every listing and change
// has to stay inside its own (docs/formats.md §7).
const LabelHome = "rig.home"

// ErrOtherHome: the object was made by the daemon of another rig home.
var ErrOtherHome = errors.New("docker object belongs to another rig home")

// Scoped is d seen from one rig home: what it creates carries the home's label, listings
// return only the home's objects, and an object of another home cannot be inspected or
// changed. Objects made before homes were labelled are returned only to a lookup by a
// named ticket, never to a scan, so no scan of one home can remove another's.
func Scoped(d Docker, home string) Docker { return scoped{Docker: d, home: home} }

type scoped struct {
	Docker
	home string
}

func (s scoped) withHome(labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	out[LabelHome] = s.home
	return out
}

func (s scoped) visible(labels, filter map[string]string) bool {
	switch labels[LabelHome] {
	case s.home:
		return true
	case "":
		return filter[LabelTicket] != ""
	}
	return false
}

func (s scoped) own(labels map[string]string, kind, name string) error {
	if h := labels[LabelHome]; h != "" && h != s.home {
		return fmt.Errorf("%w: %s %s was made by the rig home %s", ErrOtherHome, kind, name, h)
	}
	return nil
}

func (s scoped) CreateNetwork(ctx context.Context, spec NetworkSpec) (NetworkInfo, error) {
	spec.Labels = s.withHome(spec.Labels)
	return s.Docker.CreateNetwork(ctx, spec)
}

func (s scoped) CreateContainer(ctx context.Context, spec ContainerSpec) (ContainerInfo, error) {
	spec.Labels = s.withHome(spec.Labels)
	return s.Docker.CreateContainer(ctx, spec)
}

func (s scoped) BuildImage(ctx context.Context, spec BuildSpec) error {
	spec.Labels = s.withHome(spec.Labels)
	return s.Docker.BuildImage(ctx, spec)
}

func (s scoped) InspectNetwork(ctx context.Context, nameOrID string) (NetworkInfo, error) {
	info, err := s.Docker.InspectNetwork(ctx, nameOrID)
	if err != nil {
		return info, err
	}
	if err := s.own(info.Labels, "network", nameOrID); err != nil {
		return NetworkInfo{}, err
	}
	return info, nil
}

func (s scoped) InspectContainer(ctx context.Context, nameOrID string) (ContainerInfo, error) {
	info, err := s.Docker.InspectContainer(ctx, nameOrID)
	if err != nil {
		return info, err
	}
	if err := s.own(info.Labels, "container", nameOrID); err != nil {
		return ContainerInfo{}, err
	}
	return info, nil
}

func (s scoped) ListNetworks(ctx context.Context, labels map[string]string) ([]NetworkInfo, error) {
	list, err := s.Docker.ListNetworks(ctx, labels)
	var out []NetworkInfo
	for _, n := range list {
		if s.visible(n.Labels, labels) {
			out = append(out, n)
		}
	}
	return out, err
}

func (s scoped) ListContainers(ctx context.Context, labels map[string]string) ([]ContainerInfo, error) {
	list, err := s.Docker.ListContainers(ctx, labels)
	var out []ContainerInfo
	for _, c := range list {
		if s.visible(c.Labels, labels) {
			out = append(out, c)
		}
	}
	return out, err
}

// refuseOthers stops a change to an object of another home; anything else (an object
// that does not exist, Docker being down) is left for the real call to report.
func (s scoped) refuseOthers(ctx context.Context, container, network string) error {
	if container != "" {
		if _, err := s.InspectContainer(ctx, container); errors.Is(err, ErrOtherHome) {
			return err
		}
	}
	if network != "" {
		if _, err := s.InspectNetwork(ctx, network); errors.Is(err, ErrOtherHome) {
			return err
		}
	}
	return nil
}

func (s scoped) RemoveNetwork(ctx context.Context, nameOrID string) error {
	if err := s.refuseOthers(ctx, "", nameOrID); err != nil {
		return err
	}
	return s.Docker.RemoveNetwork(ctx, nameOrID)
}

func (s scoped) ConnectNetwork(ctx context.Context, container, network string, aliases []string) error {
	if err := s.refuseOthers(ctx, container, network); err != nil {
		return err
	}
	return s.Docker.ConnectNetwork(ctx, container, network, aliases)
}

func (s scoped) StartContainer(ctx context.Context, nameOrID string) error {
	if err := s.refuseOthers(ctx, nameOrID, ""); err != nil {
		return err
	}
	return s.Docker.StartContainer(ctx, nameOrID)
}

func (s scoped) StopContainer(ctx context.Context, nameOrID string, grace time.Duration) error {
	if err := s.refuseOthers(ctx, nameOrID, ""); err != nil {
		return err
	}
	return s.Docker.StopContainer(ctx, nameOrID, grace)
}

func (s scoped) RemoveContainer(ctx context.Context, nameOrID string, force bool) error {
	if err := s.refuseOthers(ctx, nameOrID, ""); err != nil {
		return err
	}
	return s.Docker.RemoveContainer(ctx, nameOrID, force)
}

func (s scoped) Exec(ctx context.Context, nameOrID string, spec ExecSpec) (ExecResult, error) {
	if err := s.refuseOthers(ctx, nameOrID, ""); err != nil {
		return ExecResult{}, err
	}
	return s.Docker.Exec(ctx, nameOrID, spec)
}
