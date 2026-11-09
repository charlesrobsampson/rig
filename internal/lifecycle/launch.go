package lifecycle

import (
	"fmt"
	"path/filepath"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/harness"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/state"
)

// previewLaunches builds each service's agent launch exactly as a real start would,
// with every secret value replaced by a placeholder, and runs nothing.
func (m *Manager) previewLaunches(t domain.Ticket, st state.State, ticketDir string, sandbox domain.Sandbox) ([]api.LaunchInfo, error) {
	var launches []api.LaunchInfo
	for _, svc := range t.Services {
		eff := policy.Resolve(m.opts.Global, t, svc, policy.Flags{Sandbox: sandbox})
		h, err := harness.Get(eff.Harness)
		if err != nil {
			return nil, invalid("service %s: %v", svc.Name, err)
		}
		req := harness.Request{
			Ticket: t, Service: svc.Name, Phase: st.Phase, TicketDir: ticketDir,
			Dir: filepath.Join(ticketDir, svc.Path), Network: eff.NetworkMode,
		}
		placeholders := map[string]string{}
		for _, name := range t.Secrets {
			placeholders[name] = secretPlaceholder(name)
		}
		if eff.Auth != "" {
			profile, ok := m.opts.Global.Auth.Profiles[eff.Auth]
			if !ok {
				return nil, invalid("service %s: auth profile %q is not defined", svc.Name, eff.Auth)
			}
			for _, name := range profile.Secrets {
				placeholders[name] = secretPlaceholder(name)
			}
			req.Profile = &profile
		}
		req.Secrets = placeholders

		launch, err := h.Command(req)
		if err != nil {
			return nil, invalid("service %s: %v", svc.Name, err)
		}
		launches = append(launches, api.LaunchInfo{Service: svc.Name, Harness: h.Name(), Dir: launch.Dir, Command: launch.Command, Env: launch.Env})
	}
	return launches, nil
}

func secretPlaceholder(name string) string { return fmt.Sprintf("<secret %s>", name) }
