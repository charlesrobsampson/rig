package lifecycle

import (
	"context"
	"fmt"
	"os"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/policy"
	rigsecrets "github.com/charlesrobsampson/rig/internal/secrets"
)

// readPlacedFile reads a file to place in a checkout, refusing one that holds the value
// of a secret the agent is given: placed files are meant to reference secrets, never
// carry them.
func readPlacedFile(f domain.PlacedFile, secrets map[string]string) ([]byte, error) {
	info, err := os.Stat(f.From)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", f.From)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("%s is larger than 1 MB", f.From)
	}
	data, err := os.ReadFile(f.From)
	if err != nil {
		return nil, err
	}
	if name, leaked := rigsecrets.Leaked(data, secrets); leaked {
		return nil, fmt.Errorf("%s contains the value of the secret %s; reference it instead (for example ${%s})", f.From, name, name)
	}
	return data, nil
}

// placeFiles copies each service's files into its checkout, ignored by git. It runs at
// every start, so a changed source reaches the next agent.
func (m *Manager) placeFiles(ctx context.Context, l loaded, infos []api.CheckoutInfo, sandbox domain.Sandbox) error {
	for i, svc := range l.ticket.Services {
		if len(svc.Files) == 0 {
			continue
		}
		eff := policy.Resolve(m.opts.Global, l.ticket, svc, policy.Flags{Sandbox: sandbox})
		_, req, err := m.agentRequest(l, svc, eff)
		if err != nil {
			return invalid("service %s: %v", svc.Name, err)
		}
		co, err := git.Open(ctx, git.Spec{Repo: svc.Repo, Branch: svc.Branch, Path: infos[i].Path, Kind: infos[i].Kind})
		if err != nil {
			return fmt.Errorf("service %s: open the checkout: %w", svc.Name, err)
		}
		for j, f := range svc.Files {
			data, err := readPlacedFile(f, req.Secrets)
			if err != nil {
				return invalid("service %s: files[%d]: %v", svc.Name, j, err)
			}
			if err := co.Place(ctx, f.Path, data); err != nil {
				return invalid("service %s: files[%d]: place %s: %v", svc.Name, j, f.Path, err)
			}
		}
	}
	return nil
}
