package lifecycle

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/state"
)

type stopTarget struct {
	svc      domain.Service
	co       git.Checkout // nil when the directory is already gone
	imported bool
}

// Stop tears a ticket down in a fixed order, never losing work silently:
//
//  1. look first: import every clone's branch into the main repository and check what
//     deleting the checkouts would lose; if work is at risk (and not forced) refuse here,
//     before anything has been touched;
//  2. agents (panes and containers), service containers and their ports, dependencies,
//     the egress proxy, any other container labelled for the ticket, then its networks;
//  3. import again (the agents have stopped, so this is their final work) and check again;
//  4. the checkouts.
//
// If a step fails the others are still tried, state.json records what is gone, and the
// error lists what failed and what remains; the checkouts are kept, since a container
// that could not be stopped may still be using them. Running stop again retries. A
// ticket that is already stopped is a no-op.
func (m *Manager) Stop(ctx context.Context, p api.TicketStopParams) (api.TicketStopResult, error) {
	l, unlock, err := m.begin(p.ID)
	if err != nil {
		return api.TicketStopResult{}, err
	}
	defer unlock()
	t, st, statePath := l.ticket, l.state, l.statePath

	targets, unrecognized := m.stopTargets(ctx, t, st)
	if len(targets) == 0 && len(unrecognized) == 0 {
		if m.alreadyStopped(ctx, p.ID, t, st) {
			res, err := m.reg.Get(p.ID)
			if err != nil {
				return api.TicketStopResult{}, err
			}
			return api.TicketStopResult{Summary: res.Summary}, nil
		}
		return api.TicketStopResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("ticket %q has nothing to stop (no checkouts)", p.ID)}
	}
	if len(unrecognized) > 0 {
		// Not a checkout rig made: never delete it, whatever force says.
		return api.TicketStopResult{}, &api.Error{Code: api.CodeBlocked, Message: fmt.Sprintf("ticket %q has directories rig will not delete", p.ID), Details: unrecognized}
	}

	// Look before touching anything: a refused stop must leave agents and containers running.
	if atRisk := m.importAndInspect(ctx, targets); len(atRisk) > 0 && !p.Force && !p.KeepCheckouts {
		return api.TicketStopResult{}, &api.Error{
			Code:    api.CodeBlocked,
			Message: fmt.Sprintf("ticket %q still holds work; commit it (or ask its agent to), or stop with force to discard it. Nothing was stopped", p.ID),
			Details: atRisk,
		}
	}

	removed, failures := m.teardown(ctx, p.ID, l, p.Force)
	m.syncTornDown(ctx, l)
	if len(failures) > 0 {
		return api.TicketStopResult{}, m.partialStop(ctx, p.ID, failures)
	}

	// The agents have stopped: import their final work, and check once more.
	if atRisk := m.importAndInspect(ctx, targets); len(atRisk) > 0 && !p.Force && !p.KeepCheckouts {
		return api.TicketStopResult{}, &api.Error{
			Code:    api.CodeBlocked,
			Message: fmt.Sprintf("ticket %q's containers are stopped, but its checkouts still hold work; resolve it, or stop with force to discard it", p.ID),
			Details: atRisk,
		}
	}

	result := api.TicketStopResult{Resources: removed}
	var checkoutFailures []string
	removedCheckout := map[string]bool{}
	for _, tg := range targets {
		info := api.StopInfo{Service: tg.svc.Name, Imported: tg.imported}
		switch {
		case p.KeepCheckouts:
		case tg.co == nil:
			info.Removed = true
		default:
			if err := tg.co.Remove(ctx, p.Force); err != nil {
				checkoutFailures = append(checkoutFailures, fmt.Sprintf("%s: %v", tg.svc.Name, err))
				result.Services = append(result.Services, info)
				continue
			}
			info.Removed = true
		}
		removedCheckout[tg.svc.Name] = info.Removed
		result.Services = append(result.Services, info)
	}

	changed, err := m.recordStopped(statePath, targets, removedCheckout)
	if err != nil {
		return api.TicketStopResult{}, err
	}
	for _, name := range changed {
		m.publish(api.EventServiceStatus, p.ID, name, api.ServiceStatusData{Status: domain.ServiceStopped})
	}
	if len(changed) > 0 {
		m.publish(api.EventTicketStopped, p.ID, "", api.EmptyData{})
	}
	if len(checkoutFailures) > 0 {
		return api.TicketStopResult{}, &api.Error{Code: api.CodeInternal, Message: "some checkouts could not be removed", Details: checkoutFailures}
	}

	res, err := m.reg.Get(p.ID)
	if err != nil {
		return api.TicketStopResult{}, err
	}
	result.Summary = res.Summary
	return result, nil
}

// stopTargets finds each service's recorded checkout and reattaches to it. A directory
// that is not the checkout state.json says it is comes back in unrecognized.
func (m *Manager) stopTargets(ctx context.Context, t domain.Ticket, st state.State) (targets []stopTarget, unrecognized []string) {
	for _, svc := range t.Services {
		rec := st.Services[svc.Name].Checkout
		if rec.Path == "" {
			continue
		}
		if _, err := os.Stat(rec.Path); os.IsNotExist(err) {
			targets = append(targets, stopTarget{svc: svc})
			continue
		}
		co, err := git.Open(ctx, git.Spec{Repo: svc.Repo, Branch: svc.Branch, Path: rec.Path, Kind: rec.Kind})
		if err != nil {
			unrecognized = append(unrecognized, fmt.Sprintf("%s: %v", svc.Name, err))
			continue
		}
		targets = append(targets, stopTarget{svc: svc, co: co})
	}
	return targets, unrecognized
}

// importAndInspect imports every clone's branch and lists, per service, the work that
// deleting the checkout would still lose.
func (m *Manager) importAndInspect(ctx context.Context, targets []stopTarget) (atRisk []string) {
	for i := range targets {
		tg := &targets[i]
		if tg.co == nil {
			continue
		}
		var importErr error
		if tg.co.Kind() == domain.CheckoutClone {
			if importErr = tg.co.Import(ctx, false); importErr == nil {
				tg.imported = true
			}
		}
		d, err := tg.co.IsDirty(ctx)
		switch {
		case err != nil:
			atRisk = append(atRisk, fmt.Sprintf("%s: cannot inspect the checkout: %v", tg.svc.Name, err))
		case d.Any():
			detail := fmt.Sprintf("%s: %s", tg.svc.Name, d.Describe())
			if importErr != nil {
				reason := strings.TrimSuffix(importErr.Error(), "; import with force to overwrite it")
				detail += fmt.Sprintf(" (the import failed: %s)", reason)
			}
			atRisk = append(atRisk, detail)
		}
	}
	return atRisk
}

// recordStopped marks every target stopped and forgets the checkouts that were
// removed. It returns the services whose status changed.
func (m *Manager) recordStopped(path string, targets []stopTarget, removed map[string]bool) ([]string, error) {
	var changed []string
	err := m.opts.States.Update(path, func(s *state.State) error {
		changed = nil
		if m.opts.Docker != nil {
			s.Network, s.EgressNetwork = state.Network{}, nil
			for name := range s.Dependencies {
				s.Dependencies[name] = state.DependencyState{Status: domain.ServiceStopped}
			}
		}
		for _, tg := range targets {
			ss := s.Services[tg.svc.Name]
			if removed[tg.svc.Name] {
				ss.Checkout = state.Checkout{}
			}
			if m.opts.Docker != nil {
				ss.ContainerID, ss.HostPort = "", 0
			}
			if ss.Status != domain.ServiceStopped {
				ss.Status = domain.ServiceStopped
				changed = append(changed, tg.svc.Name)
			}
			s.Services[tg.svc.Name] = ss
		}
		return nil
	})
	if err != nil {
		return nil, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record stop: %v", err)}
	}
	return changed, nil
}
