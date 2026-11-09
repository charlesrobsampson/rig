package lifecycle

import (
	"sort"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/policy"
)

// checkSecrets refuses a start that could not launch its agents or services, before
// anything is created: an auth profile that is not defined, a host_login profile for a
// container sandbox, and (when a resolver is wired) any secret that has no value. The
// error names what is missing, never a value.
func (m *Manager) checkSecrets(l loaded, sandbox domain.Sandbox) error {
	names := map[string]bool{}
	for _, name := range l.ticket.Secrets {
		names[name] = true
	}
	for _, svc := range l.ticket.Services {
		eff := policy.Resolve(m.opts.Global, l.ticket, svc, policy.Flags{Sandbox: sandbox})
		if eff.Auth == "" {
			continue
		}
		profile, ok := m.opts.Global.Auth.Profiles[eff.Auth]
		if !ok {
			return invalid("service %s: auth profile %q is not defined", svc.Name, eff.Auth)
		}
		if profile.Type == domain.AuthHostLogin && eff.Sandbox == domain.SandboxContainer {
			return invalid("service %s: auth profile %q uses the machine's own login, which a container cannot have; use a token, API key or cloud profile, or sandbox: host", svc.Name, eff.Auth)
		}
		for _, name := range profile.Secrets {
			names[name] = true
		}
	}
	if m.opts.Secrets == nil || len(names) == 0 {
		return nil
	}
	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}
	sort.Strings(list)
	if _, err := m.opts.Secrets(list); err != nil {
		return invalid("ticket %q: %v", l.id, err)
	}
	return nil
}
