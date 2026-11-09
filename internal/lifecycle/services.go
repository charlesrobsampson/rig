package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/ports"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

// logTail is how many of a container's last log lines an error carries.
const logTail = 20

func ContainerName(ticket, service string) string { return "rig-" + ticket + "-" + service }

func (m *Manager) imageTag(ticket, service string) string {
	return ContainerName(ticket, service) + ":latest"
}

// runsInDocker says whether a service gets a service container: it has a Dockerfile.
func runsInDocker(svc domain.Service) bool { return svc.Build.Dockerfile != "" }

// validateServices checks everything about the services' container setup that can be
// checked before anything is created, so a mistake costs no networks, ports or images.
func (m *Manager) validateServices(l loaded, checkouts []api.CheckoutInfo) error {
	if m.opts.Docker == nil {
		return nil
	}
	for i, svc := range l.ticket.Services {
		if !runsInDocker(svc) {
			continue
		}
		path := filepath.Join(checkouts[i].Path, svc.Build.Dockerfile)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return invalid("service %s: no Dockerfile at %s", svc.Name, path)
		}
		if svc.Healthcheck != "" && !strings.HasPrefix(svc.Healthcheck, "/") {
			return invalid("service %s: healthcheck %q must be a URL path starting with /", svc.Name, svc.Healthcheck)
		}
		if svc.Healthcheck != "" && svc.InternalPort == 0 {
			return invalid("service %s has a healthcheck but no internal_port to check it on", svc.Name)
		}
	}
	return nil
}

// portUsable is the allocator's probe: free on this machine, or held by a container rig
// itself started for a service (its own, or the proxy that carries it in internal
// mode); after a restart that container still holds the port it was given.
func (m *Manager) portUsable(port int) bool {
	for _, role := range []string{docker.RoleService, docker.RoleProxy} {
		list, err := m.opts.Docker.ListContainers(context.Background(), map[string]string{docker.LabelRole: role})
		if err != nil {
			continue
		}
		for _, c := range list {
			if c.State != docker.StateRunning {
				continue
			}
			for _, p := range c.Ports {
				if p.Host == port {
					return true
				}
			}
		}
	}
	return ports.ListenProbe(port)
}

// startServices builds and runs a container for each service that has a Dockerfile.
// One that fails is reported (and its status recorded as error); the others carry on.
func (m *Manager) startServices(ctx context.Context, l loaded, checkouts []api.CheckoutInfo, net *api.NetworkInfo, extraEnv map[string]string) []api.ServiceContainerInfo {
	if m.opts.Docker == nil || net == nil {
		return nil
	}
	var out []api.ServiceContainerInfo
	for i, svc := range l.ticket.Services {
		if !runsInDocker(svc) {
			if svc.Start != "" {
				out = append(out, api.ServiceContainerInfo{Service: svc.Name, Note: "not run: it has a start command but no build.dockerfile (rig runs services from their Dockerfile)"})
			}
			continue
		}
		info := m.startService(ctx, l, svc, checkouts[i].Path, net, extraEnv)
		out = append(out, info)
	}
	return out
}

func (m *Manager) startService(ctx context.Context, l loaded, svc domain.Service, checkout string, net *api.NetworkInfo, extraEnv map[string]string) api.ServiceContainerInfo {
	name := ContainerName(l.id, svc.Name)
	info := api.ServiceContainerInfo{Service: svc.Name, Name: name}

	fail := func(err error, withLogs bool) api.ServiceContainerInfo {
		msg := err.Error()
		if withLogs {
			if tail := m.logTail(ctx, name); tail != "" {
				msg += "\nlast log lines:\n" + tail
			}
		}
		info.Error = msg
		m.recordService(l, svc.Name, "", domain.ServiceError, msg)
		return info
	}

	// A container that is up is left alone; one that stopped is replaced.
	if existing, err := m.opts.Docker.InspectContainer(ctx, name); err == nil {
		if existing.State == docker.StateRunning {
			info.ID, info.Running, info.Note = existing.ID, true, "already running"
			for _, p := range existing.Ports {
				info.HostPort = p.Host
			}
			if info.HostPort == 0 { // an internal-mode service's port is carried by the proxy
				info.HostPort = l.state.Services[svc.Name].HostPort
			}
			info.URL = hostURL(info.HostPort)
			return info
		}
		if err := m.opts.Docker.RemoveContainer(ctx, name, true); err != nil {
			return fail(fmt.Errorf("replace the stopped container: %w", err), false)
		}
	}

	hostPort := 0
	if svc.InternalPort > 0 {
		p, err := m.ports.Allocate(ctx, l.id, svc.Name)
		if err != nil {
			return fail(fmt.Errorf("allocate a host port: %w", err), false)
		}
		hostPort = p
	}

	tag := m.imageTag(l.id, svc.Name)
	err := m.opts.Docker.BuildImage(ctx, docker.BuildSpec{
		Tag: tag, ContextDir: checkout, Dockerfile: svc.Build.Dockerfile,
		Labels: docker.Labels(l.id, svc.Name, docker.RoleService),
	})
	if err != nil {
		return fail(fmt.Errorf("build the image: %w", err), false)
	}

	env, err := m.serviceEnv(l)
	if err != nil {
		return fail(err, false)
	}
	for k, v := range extraEnv { // the proxy settings (internal mode)
		env[k] = v
	}
	spec := docker.ContainerSpec{
		Name: name, Image: tag, Env: env, Labels: docker.Labels(l.id, svc.Name, docker.RoleService),
		WorkDir: "/work", Mounts: []docker.Mount{{Source: checkout, Target: "/work"}},
		Network: net.Name, Aliases: []string{svc.Name},
	}
	if svc.Start != "" {
		spec.Command = []string{"sh", "-c", svc.Start}
	}
	// On an internal network Docker cannot publish a port; the proxy container does it.
	if hostPort != 0 && !net.Internal {
		spec.Ports = []docker.PortBinding{{Container: svc.InternalPort, Host: hostPort}}
	}
	created, err := m.opts.Docker.CreateContainer(ctx, spec)
	if err != nil {
		return fail(fmt.Errorf("create the container: %w", err), false)
	}
	if err := m.opts.Docker.StartContainer(ctx, created.ID); err != nil {
		m.opts.Docker.RemoveContainer(context.WithoutCancel(ctx), created.ID, true)
		return fail(fmt.Errorf("start the container: %w", err), false)
	}
	info.ID, info.HostPort, info.URL = created.ID, hostPort, hostURL(hostPort)

	if err := m.waitHealthy(ctx, created.ID, svc, hostPort); err != nil {
		info.Running = false
		return fail(err, true)
	}
	info.Running = true
	m.recordService(l, svc.Name, created.ID, domain.ServiceRunning, "")
	return info
}

func hostURL(port int) string {
	if port == 0 {
		return ""
	}
	return "http://localhost:" + strconv.Itoa(port)
}

// serviceEnv is what a service container is told: the ticket's env, the values of the
// secrets the ticket lists, and who it is.
func (m *Manager) serviceEnv(l loaded) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range l.ticket.Env {
		env[k] = v
	}
	if len(l.ticket.Secrets) > 0 {
		if m.opts.Secrets == nil {
			return nil, fmt.Errorf("secrets %s cannot be resolved yet", strings.Join(l.ticket.Secrets, ", "))
		}
		values, err := m.opts.Secrets(l.ticket.Secrets)
		if err != nil {
			return nil, fmt.Errorf("resolve secrets: %w", err)
		}
		for _, name := range l.ticket.Secrets {
			env[name] = values[name]
		}
	}
	env["RIG_TICKET"] = l.id
	return env, nil
}

// waitHealthy returns once the service is up: its healthcheck URL answers, or, with no
// healthcheck, it has stayed running for the settle time. It fails early if the
// container exits.
func (m *Manager) waitHealthy(ctx context.Context, id string, svc domain.Service, hostPort int) error {
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
	probe := m.opts.HealthProbe
	if probe == nil {
		probe = httpProbe
	}
	url := ""
	if svc.Healthcheck != "" {
		url = hostURL(hostPort) + svc.Healthcheck
	}

	deadline := m.opts.Clock.After(timeout)
	settled := m.opts.Clock.After(settle)
	for {
		c, err := m.opts.Docker.InspectContainer(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect the container: %w", err)
		}
		if c.State != docker.StateRunning {
			return fmt.Errorf("the container exited with code %d before becoming healthy", c.ExitCode)
		}
		if url != "" && probe(ctx, url) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			if url == "" {
				return nil // it never crashed; with nothing to check, that is as good as it gets
			}
			return fmt.Errorf("not healthy after %s (%s did not answer)", timeout, url)
		case <-settled:
			if url == "" {
				return nil
			}
			settled = nil
		case <-m.opts.Clock.After(poll):
		}
	}
}

func httpProbe(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode < 400
}

func (m *Manager) logTail(ctx context.Context, name string) string {
	r, err := m.opts.Docker.Logs(context.WithoutCancel(ctx), name, docker.LogOptions{Tail: logTail})
	if err != nil {
		return ""
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return strings.TrimRight(string(b), "\n")
}

// recordService writes a service's container id and status, publishing the status
// change. A service that stops running also gives up its host port.
func (m *Manager) recordService(l loaded, service, containerID string, status domain.ServiceStatus, errMsg string) {
	var before domain.ServiceStatus
	err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		ss := s.Services[service]
		before = ss.Status
		ss.ContainerID, ss.Status, ss.Error = containerID, status, errMsg
		s.Services[service] = ss
		return nil
	})
	if err != nil {
		return
	}
	if before != status {
		m.publish(api.EventServiceStatus, l.id, service, api.ServiceStatusData{Status: status})
	}
}

// removeServiceContainers stops and removes the ticket's service containers and
// releases their ports. Containers already gone are fine. It tries every service and
// returns every failure, so one stuck container does not hide the others.
func (m *Manager) removeServiceContainers(ctx context.Context, id string, t domain.Ticket, st state.State) error {
	if m.opts.Docker == nil {
		return nil
	}
	grace := m.opts.StopGrace
	if grace == 0 {
		grace = 10 * time.Second
	}
	var failures []error
	for _, svc := range t.Services {
		if !runsInDocker(svc) {
			continue
		}
		name := ContainerName(id, svc.Name)
		if err := m.opts.Docker.StopContainer(ctx, name, grace); err != nil && !errors.Is(err, docker.ErrNotFound) {
			failures = append(failures, dockerError(fmt.Sprintf("service %s: stop its container", svc.Name), err))
			continue
		}
		if err := m.opts.Docker.RemoveContainer(ctx, name, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
			failures = append(failures, dockerError(fmt.Sprintf("service %s: remove its container", svc.Name), err))
			continue
		}
		if m.ports != nil && st.Services[svc.Name].HostPort != 0 {
			if err := m.ports.Release(id, svc.Name); err != nil {
				failures = append(failures, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("service %s: release its host port: %v", svc.Name, err)})
			}
		}
	}
	return errors.Join(failures...)
}
