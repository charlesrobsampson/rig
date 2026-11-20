package lifecycle

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/policy"
)

// agentConfigDir is where an agent's own harness configuration lives on the host.
func agentConfigDir(ticketDir, service string) string {
	return filepath.Join(ticketDir, ".rig-claude", service)
}

// trustablePaths resolves paths to the real paths a harness keys trust by, refusing
// any that leave root: rig only trusts what it created for a ticket.
func trustablePaths(root string, paths ...string) ([]string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve the tickets directory: %w", err)
	}
	var out []string
	for _, p := range paths {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", p, err)
		}
		if !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s is outside the tickets directory %s; rig will not mark it trusted", real, realRoot)
		}
		out = append(out, real)
	}
	return out, nil
}

// agentSnapshotDir is where an agent's configuration templates are copied on the host.
// It sits outside the agent's config directory so a container can mount it read-only.
func agentSnapshotDir(ticketDir, service string) string {
	return filepath.Join(ticketDir, ".rig-agent-config", service)
}

// ownsConfig reports whether an agent gets its own configuration directory: every
// container agent does, and a host agent with a credential profile; host_login keeps
// the user's own, because that is where the machine's login is.
func ownsConfig(sandbox domain.Sandbox, profile *config.AuthProfile) bool {
	return sandbox == domain.SandboxContainer || (profile != nil && profile.Type != domain.AuthHostLogin)
}

// checkAgentConfig refuses agent configuration and placed files (files.go) that cannot or must
// not reach an agent, before anything for the ticket is made.
func (m *Manager) checkAgentConfig(l loaded, sandbox domain.Sandbox) error {
	for _, svc := range l.ticket.Services {
		eff := policy.Resolve(m.opts.Global, l.ticket, svc, policy.Flags{Sandbox: sandbox})
		if len(eff.AgentConfig) == 0 && len(svc.Files) == 0 {
			continue
		}
		h, req, err := m.agentRequest(l, svc, eff)
		if err != nil {
			return invalid("service %s: %v", svc.Name, err)
		}
		if len(eff.AgentConfig) > 0 {
			if !ownsConfig(eff.Sandbox, req.Profile) {
				return invalid("service %s: agent_config needs the agent's own configuration, which a host agent has only with a credential profile (oauth_token, api_key, auth_token or cloud); with host_login or no profile it uses the user's own configuration", svc.Name)
			}
			if err := h.CheckTemplates(eff.AgentConfig, req.Secrets); err != nil {
				return invalid("service %s: %v", svc.Name, err)
			}
		}
		for i, f := range svc.Files {
			if _, err := readPlacedFile(f, req.Secrets); err != nil {
				return invalid("service %s: files[%d]: %v", svc.Name, i, err)
			}
		}
	}
	return nil
}
