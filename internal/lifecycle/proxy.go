package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/egress"
)

func ProxyName(ticket string) string { return "rig-" + ticket + "-proxy" }

const proxyPort = 3128

// egressAllowlist is every host the ticket's containers may reach through the proxy:
// the global defaults plus the ticket's and each service's own.
func (m *Manager) egressAllowlist(t domain.Ticket) []string {
	set := map[string]bool{}
	for _, h := range m.opts.Global.Egress.DefaultAllow {
		set[strings.ToLower(h)] = true
	}
	for _, h := range t.Egress.Allow {
		set[strings.ToLower(h)] = true
	}
	for _, svc := range t.Services {
		for _, h := range svc.Egress.Allow {
			set[strings.ToLower(h)] = true
		}
	}
	list := make([]string, 0, len(set))
	for h := range set {
		list = append(list, h)
	}
	sort.Strings(list)
	return list
}

func (m *Manager) egressPorts() []int {
	if p := m.opts.Global.Egress.DefaultPorts; len(p) > 0 {
		return p
	}
	return []int{443}
}

// startProxy runs the ticket's egress proxy in internal mode and returns the
// environment that points the ticket's containers at it. The proxy is the only way out,
// so a proxy that cannot start fails the whole start rather than leaving containers
// without containment. Bridge mode has no proxy and no environment.
func (m *Manager) startProxy(ctx context.Context, l loaded, net *api.NetworkInfo) (map[string]string, error) {
	if m.opts.Docker == nil || net == nil || !net.Internal {
		return nil, nil
	}
	if err := (egress.Policy{Hosts: m.egressAllowlist(l.ticket), Ports: m.egressPorts()}).Validate(); err != nil {
		return nil, invalid("ticket %q: egress allowlist: %v", l.id, err)
	}
	name := ProxyName(l.id)
	ports := make([]string, 0, len(m.egressPorts()))
	for _, p := range m.egressPorts() {
		ports = append(ports, strconv.Itoa(p))
	}
	// Docker cannot publish ports of containers that are only on an internal network, so
	// the proxy carries each service's host port inward (see egress.Forward).
	forwards, err := m.serviceForwards(ctx, l)
	if err != nil {
		return nil, err
	}
	env := map[string]string{
		"ALLOW_HOSTS": strings.Join(m.egressAllowlist(l.ticket), ","),
		"ALLOW_PORTS": strings.Join(ports, ","),
		"LISTEN":      ":" + strconv.Itoa(proxyPort),
	}
	var published []docker.PortBinding
	if len(forwards) > 0 {
		env["FORWARD"] = egress.FormatForwards(forwards)
		for _, f := range forwards {
			published = append(published, docker.PortBinding{Container: f.ListenPort, Host: f.ListenPort})
		}
	}

	reuse := false
	if existing, err := m.opts.Docker.InspectContainer(ctx, name); err == nil {
		// A running proxy with the same allowlist is kept; one whose allowlist changed
		// (ticket.yaml was edited) is replaced so the new rules apply.
		if existing.State == docker.StateRunning && existing.Env["ALLOW_HOSTS"] == env["ALLOW_HOSTS"] && existing.Env["ALLOW_PORTS"] == env["ALLOW_PORTS"] && existing.Env["FORWARD"] == env["FORWARD"] {
			reuse = true
		} else if err := m.opts.Docker.RemoveContainer(ctx, name, true); err != nil {
			return nil, dockerError(fmt.Sprintf("ticket %q: replace the egress proxy", l.id), err)
		}
	}
	if !reuse {
		tag, _, err := m.egressImages.Ensure(ctx)
		if err != nil {
			return nil, dockerError(fmt.Sprintf("ticket %q: the egress proxy image", l.id), err)
		}
		// Created on the egress network (a normal bridge: its way out), then joined to
		// the internal network, where the ticket's containers reach it.
		created, err := m.opts.Docker.CreateContainer(ctx, docker.ContainerSpec{
			Name: name, Image: tag, Env: env, Labels: docker.Labels(l.id, "", docker.RoleProxy), Network: net.Egress, Ports: published,
		})
		if err != nil {
			return nil, dockerError(fmt.Sprintf("ticket %q: create the egress proxy", l.id), err)
		}
		fail := func(msg string, err error) (map[string]string, error) {
			m.opts.Docker.RemoveContainer(context.WithoutCancel(ctx), created.ID, true)
			return nil, dockerError(fmt.Sprintf("ticket %q: %s", l.id, msg), err)
		}
		if err := m.opts.Docker.ConnectNetwork(ctx, created.ID, net.Name, []string{"proxy"}); err != nil {
			return fail("join the egress proxy to the ticket network", err)
		}
		if err := m.opts.Docker.StartContainer(ctx, created.ID); err != nil {
			return fail("start the egress proxy", err)
		}
		if err := m.waitDependencyUp(ctx, created.ID); err != nil {
			msg := fmt.Sprintf("ticket %q: the egress proxy: %v", l.id, err)
			if tail := m.logTail(ctx, name); tail != "" {
				msg += "\nlast log lines:\n" + tail
			}
			m.opts.Docker.RemoveContainer(context.WithoutCancel(ctx), created.ID, true)
			return nil, &api.Error{Code: api.CodeUnavailable, Message: msg}
		}
	}
	return m.egressEnv(l.ticket, name), nil
}

// egressEnv points a container at the proxy and keeps traffic inside the ticket's
// network (services, dependencies, the proxy itself) off it.
func (m *Manager) egressEnv(t domain.Ticket, proxy string) map[string]string {
	url := "http://" + proxy + ":" + strconv.Itoa(proxyPort)
	noProxy := []string{"localhost", "127.0.0.1", proxy}
	for _, svc := range t.Services {
		noProxy = append(noProxy, svc.Name, ContainerName(t.ID, svc.Name), AgentContainerName(t.ID, svc.Name))
	}
	for _, dep := range t.Dependencies {
		noProxy = append(noProxy, dep.Name, DependencyName(t.ID, dep.Name))
	}
	list := strings.Join(noProxy, ",")
	return map[string]string{
		"HTTPS_PROXY": url, "https_proxy": url, "HTTP_PROXY": url, "http_proxy": url,
		"NO_PROXY": list, "no_proxy": list,
	}
}

// removeProxy removes the ticket's egress proxy; one that is already gone is fine.
func (m *Manager) removeProxy(ctx context.Context, id string) error {
	if m.opts.Docker == nil {
		return nil
	}
	grace := m.opts.StopGrace
	if grace == 0 {
		grace = 5 * time.Second
	}
	name := ProxyName(id)
	if err := m.opts.Docker.StopContainer(ctx, name, grace); err != nil && !errors.Is(err, docker.ErrNotFound) {
		return dockerError(fmt.Sprintf("ticket %q: stop the egress proxy", id), err)
	}
	if err := m.opts.Docker.RemoveContainer(ctx, name, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
		return dockerError(fmt.Sprintf("ticket %q: remove the egress proxy", id), err)
	}
	return nil
}

// serviceForwards lists the host ports of the ticket's services that run in Docker,
// allocating them now (the proxy needs them to be created, before the services).
func (m *Manager) serviceForwards(ctx context.Context, l loaded) ([]egress.Forward, error) {
	var forwards []egress.Forward
	for _, svc := range l.ticket.Services {
		if !runsInDocker(svc) || svc.InternalPort == 0 {
			continue
		}
		port, err := m.ports.Allocate(ctx, l.id, svc.Name)
		if err != nil {
			return nil, &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("service %s: allocate a host port: %v", svc.Name, err)}
		}
		if port == proxyPort {
			return nil, invalid("service %s: host port %d is the egress proxy's own port; change docker.port_range", svc.Name, port)
		}
		forwards = append(forwards, egress.Forward{ListenPort: port, Target: svc.Name + ":" + strconv.Itoa(svc.InternalPort)})
	}
	return forwards, nil
}
