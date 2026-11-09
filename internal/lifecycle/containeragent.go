package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/supervisor"
)

func AgentContainerName(ticket, service string) string {
	return ContainerName(ticket, service) + "-agent"
}

const (
	// Where an agent sees its checkout, the ticket's working files, and its hook events.
	agentWorkDir   = "/work"
	agentTicketDir = "/rig/ticket"
	agentEventsDir = "/rig-events"
	eventsFile     = "events.log"
)

// ticketFiles are the only files of the ticket directory an agent gets: the ones it
// works from and writes to. state.json and ticket.yaml stay on the host.
var ticketFiles = []string{"plan.md", "tasks.md", "manual-tests.md", "contract.md"}

// EventsDir is where a container agent's hooks append their state reports, on the host.
func EventsDir(ticketDir, service string) string {
	return filepath.Dir(supervisor.EventsFile(filepath.Dir(ticketDir), filepath.Base(ticketDir), service))
}

// prepareContainerAgent makes the agent's container and returns the command that runs
// the agent inside it. The container holds exactly: the service's checkout (read-write),
// for a clone the main repository's object store (read-only, at its real path, because
// the clone reads it through alternates), the ticket's working files, and a directory
// for hook events. It joins the ticket's network. No git credentials, no Docker socket,
// no other branches.
func (m *Manager) prepareContainerAgent(ctx context.Context, l loaded, svc domain.Service, eff policy.Effective, co api.CheckoutInfo, net *api.NetworkInfo, extraEnv map[string]string) (agentLaunch, error) {
	h, req, err := m.agentRequest(l, svc, eff)
	if err != nil {
		return agentLaunch{}, err
	}

	base := m.opts.Global.Docker.AgentBase
	if base == "" {
		base = "debian:bookworm-slim"
	}
	if runsInDocker(svc) {
		base = m.imageTag(l.id, svc.Name) // the service's own toolchain
	}
	image, _, err := m.agentImages.Ensure(ctx, base)
	if err != nil {
		return agentLaunch{}, err
	}

	mounts := []docker.Mount{{Source: co.Path, Target: agentWorkDir}}
	if co.Kind == domain.CheckoutClone {
		checkout, err := git.Open(ctx, git.Spec{Repo: svc.Repo, Branch: svc.Branch, Path: co.Path, Kind: co.Kind})
		if err != nil {
			return agentLaunch{}, fmt.Errorf("open the checkout: %w", err)
		}
		if om, ok := checkout.(git.ObjectsMounter); ok {
			objects, err := om.ObjectsMounts()
			if err != nil {
				return agentLaunch{}, fmt.Errorf("find the repository's objects: %w", err)
			}
			for _, o := range objects {
				mounts = append(mounts, docker.Mount{Source: o, Target: o, ReadOnly: true})
			}
		}
	}
	for _, name := range ticketFiles {
		if info, err := os.Stat(filepath.Join(l.dir, name)); err == nil && info.Mode().IsRegular() {
			mounts = append(mounts, docker.Mount{Source: filepath.Join(l.dir, name), Target: agentTicketDir + "/" + name})
		}
	}
	events := EventsDir(l.dir, svc.Name)
	if err := os.MkdirAll(events, 0o700); err != nil {
		return agentLaunch{}, fmt.Errorf("prepare the events directory: %w", err)
	}
	mounts = append(mounts, docker.Mount{Source: events, Target: agentEventsDir})

	name := AgentContainerName(l.id, svc.Name)
	id, err := m.agentContainer(ctx, name, docker.ContainerSpec{
		Name: name, Image: image, Command: []string{"sleep", "infinity"},
		Labels: docker.Labels(l.id, svc.Name, docker.RoleAgent), WorkDir: agentWorkDir,
		User:   fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		Mounts: mounts, Network: net.Name, TTY: true, Stdin: true,
		Env: m.gitIdentity(ctx, svc.Repo),
	})
	if err != nil {
		return agentLaunch{}, err
	}

	req.Dir, req.AgentTicketDir = agentWorkDir, agentTicketDir
	// Hooks inside the container cannot reach the daemon; they append to a file the
	// daemon watches (docs/spikes/04-agent-container.md §5).
	req.HookCommand = `sh -c 'echo "$1" >> ` + agentEventsDir + "/" + eventsFile + `' rig-event`
	launch, err := h.Command(req)
	if err != nil {
		return agentLaunch{}, err
	}

	// The pane runs `docker exec -it`; the agent's environment travels in the docker
	// process's own environment (`--env NAME`), so secrets never appear on a command line.
	command := []string{"docker", "exec", "-it", "--workdir", agentWorkDir}
	merged := map[string]string{}
	for k, v := range launch.Env {
		merged[k] = v
	}
	for k, v := range extraEnv {
		merged[k] = v
	}
	for _, k := range sortedEnvKeys(merged) {
		command = append(command, "--env", k)
	}
	command = append(append(command, name), launch.Command...)
	env := map[string]string{}
	for k, v := range launch.Env {
		env[k] = v
	}
	for k, v := range extraEnv { // the proxy settings: the only way out of an internal network
		env[k] = v
	}
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} { // so the pane's docker finds the same daemon
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return agentLaunch{command: command, env: env, dir: co.Path, containerName: name, containerID: id}, nil
}

// agentContainer returns the id of the agent's running container: an existing one is
// kept, a stopped one is replaced (its image or mounts may have changed).
func (m *Manager) agentContainer(ctx context.Context, name string, spec docker.ContainerSpec) (string, error) {
	if existing, err := m.opts.Docker.InspectContainer(ctx, name); err == nil {
		if existing.State == docker.StateRunning {
			return existing.ID, nil
		}
		if err := m.opts.Docker.RemoveContainer(ctx, name, true); err != nil {
			return "", fmt.Errorf("replace the stopped agent container: %w", err)
		}
	}
	created, err := m.opts.Docker.CreateContainer(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create the agent container: %w", err)
	}
	if err := m.opts.Docker.StartContainer(ctx, created.ID); err != nil {
		m.opts.Docker.RemoveContainer(context.WithoutCancel(ctx), created.ID, true)
		return "", fmt.Errorf("start the agent container: %w", err)
	}
	return created.ID, nil
}

// gitIdentity gives the agent's commits an author: the one the repository is already
// configured with, else a neutral one. Nothing else of the host's git config goes in.
func (m *Manager) gitIdentity(ctx context.Context, repo string) map[string]string {
	get := func(key, fallback string) string {
		if out, err := git.Trusted().Run(ctx, repo, "config", "--get", key); err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
		return fallback
	}
	name, email := get("user.name", "rig agent"), get("user.email", "rig-agent@localhost")
	return map[string]string{"GIT_AUTHOR_NAME": name, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_NAME": name, "GIT_COMMITTER_EMAIL": email}
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// removeAgentContainers removes the ticket's agent containers. Already-gone ones are
// fine; an unreachable Docker is an error unless force (the checkout must not be
// removed under an agent that may still be running). Every failure is reported.
func (m *Manager) removeAgentContainers(ctx context.Context, id string, t domain.Ticket, force bool) error {
	if m.opts.Docker == nil {
		return nil
	}
	var failures []error
	for _, svc := range t.Services {
		name := AgentContainerName(id, svc.Name)
		err := m.opts.Docker.RemoveContainer(ctx, name, true)
		switch {
		case err == nil, errors.Is(err, docker.ErrNotFound), force:
		default:
			failures = append(failures, dockerError(fmt.Sprintf("agent of %s: remove its container", svc.Name), err))
		}
	}
	return errors.Join(failures...)
}
