package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/gates"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/state"
)

// SetPhase moves a ticket on behalf of a human.
func (m *Manager) SetPhase(ctx context.Context, p api.PhaseSetParams) (api.TransitionResult, error) {
	return m.transition(ctx, p.ID, "human", p.To, p.Reason, "")
}

// AskForPhase records an agent's request, left in its journal, to move the ticket, so
// the human sees it waiting. It never moves the phase or approves anything: the human
// does that, and any move clears the request.
func (m *Manager) AskForPhase(id, service string, to domain.Phase, reason string) error {
	if !to.Valid() {
		return invalid("invalid phase %q", string(to))
	}
	l, unlock, err := m.begin(id)
	if err != nil {
		return err
	}
	defer unlock()
	if !hasService(l.ticket, service) {
		return invalid("ticket %q has no service %q", id, service)
	}
	reason = strings.TrimSpace(reason)
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		if s.Phase == to {
			return invalid("ticket %q is already in %s", id, to)
		}
		s.PhaseRequest = &state.PhaseRequest{To: to, Service: service, Reason: reason, At: m.opts.Clock.Now().UTC()}
		return nil
	})
	if err != nil {
		return err
	}
	m.publish(api.EventPhaseRequested, id, service, api.PhaseRequestedData{To: to, Reason: reason})
	return nil
}

// RequestTransition is an agent asking for a move. It is judged by exactly the rules a
// human's move is: the gate for a forward step must already be approved (an agent can
// never grant one), and a backward step needs a reason.
func (m *Manager) RequestTransition(ctx context.Context, p api.TransitionRequestParams) (api.TransitionResult, error) {
	if p.Service == "" {
		return api.TransitionResult{}, invalid("a transition request must say which service's agent is asking")
	}
	return m.transition(ctx, p.ID, "agent:"+p.Service, p.To, p.Reason, p.Service)
}

func (m *Manager) transition(ctx context.Context, id, by string, to domain.Phase, reason, service string) (api.TransitionResult, error) {
	if !to.Valid() {
		return api.TransitionResult{}, invalid("invalid phase %q", string(to))
	}
	l, unlock, err := m.begin(id)
	if err != nil {
		return api.TransitionResult{}, err
	}
	defer unlock()
	res, err := m.moveLocked(l, id, by, to, reason, service)
	if err == nil && (to == domain.PhaseTest || to == domain.PhaseReview) {
		res.Warnings = m.uncommittedWork(ctx, l)
	}
	return res, err
}

// uncommittedWork names each checkout that holds changes no commit has: rig imports
// and reviews commits only, so that work would be missing from test and review. It
// warns rather than blocks, because a human may be moving on deliberately.
func (m *Manager) uncommittedWork(ctx context.Context, l loaded) []string {
	var warnings []string
	for _, svc := range l.ticket.Services {
		rec := l.state.Services[svc.Name]
		if rec.Checkout.Path == "" {
			continue
		}
		co, err := git.Open(ctx, git.Spec{Repo: svc.Repo, Branch: svc.Branch, Path: rec.Checkout.Path, Kind: rec.Checkout.Kind})
		if err != nil {
			continue
		}
		dirty, err := co.IsDirty(ctx)
		if err != nil || dirty.Modified+dirty.Untracked == 0 {
			continue
		}
		dirty.Unimported = 0
		warnings = append(warnings, fmt.Sprintf("service %s has uncommitted work (%s); rig reviews and pushes commits only, so commit it or it is left out", svc.Name, dirty.Describe()))
	}
	return warnings
}

// moveLocked is the move itself, for a caller that already holds the ticket's lock.
func (m *Manager) moveLocked(l loaded, id, by string, to domain.Phase, reason, service string) (api.TransitionResult, error) {
	if service != "" && !hasService(l.ticket, service) {
		return api.TransitionResult{}, invalid("ticket %q has no service %q", id, service)
	}

	if l.state.Phase == domain.PhasePlan && to == domain.PhaseImplement && len(gates.Missing(gates.Requirements(m.opts.Global, l.ticket, l.state), domain.GatePlanApproval)) == 0 && planGateRequired(m, l) {
		if e := m.contractAgreed(l, "leave the plan phase"); e != nil {
			return api.TransitionResult{}, e
		}
	}

	reason = strings.TrimSpace(reason)
	var made state.Transition
	err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		// Decided against the state as it is now, inside the update, so the check and the
		// write cannot be separated by another move.
		if e := m.decide(id, l.ticket, *s, to, reason); e != nil {
			return e
		}
		made = state.Transition{From: s.Phase, To: to, By: by, Reason: reason, At: m.opts.Clock.Now().UTC()}
		if state.IsBackward(s.Phase, to) {
			s.Approvals = withoutGates(s.Approvals, gates.InvalidatedByMovingTo(to))
		}
		s.Phase = to
		s.PhaseRequest = nil
		s.Transitions = append(s.Transitions, made)
		return nil
	})
	if err != nil {
		if _, ok := err.(*api.Error); ok {
			return api.TransitionResult{}, err
		}
		return api.TransitionResult{}, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record transition: %v", err)}
	}

	m.publish(api.EventPhaseChanged, id, "", api.PhaseChangedData{From: made.From, To: made.To, By: by, Reason: reason})
	res, err := m.reg.Get(id)
	if err != nil {
		return api.TransitionResult{}, err
	}
	return api.TransitionResult{Summary: res.Summary, From: made.From, To: made.To, By: by, Reason: reason, At: made.At}, nil
}

// decide says whether the move is allowed now: the order of phases, a reason for a
// backward move, and the approvals a forward move needs.
func (m *Manager) decide(id string, t domain.Ticket, st state.State, to domain.Phase, reason string) *api.Error {
	from := st.Phase
	if err := state.CanTransition(from, to); err != nil {
		return &api.Error{Code: api.CodeConflict, Message: err.Error()}
	}
	if state.IsBackward(from, to) {
		if reason == "" {
			return invalid("moving %s back to %s needs a reason", from, to)
		}
		return nil
	}
	gate, guarded := gates.GateFor(from, to)
	if !guarded {
		return nil
	}
	missing := gates.Missing(gates.Requirements(m.opts.Global, t, st), gate)
	if len(missing) == 0 {
		return nil
	}
	details := make([]string, len(missing))
	for i, svc := range missing {
		details[i] = fmt.Sprintf("%s: %s", gate, svc)
	}
	return &api.Error{
		Code:    api.CodeBlocked,
		Message: fmt.Sprintf("ticket %q cannot move %s -> %s until %s is approved (run: rig approve %s %s)", id, from, to, gate, id, gate),
		Details: details,
	}
}

func hasService(t domain.Ticket, name string) bool {
	for _, s := range t.Services {
		if s.Name == name {
			return true
		}
	}
	return false
}

// withoutGates drops the approvals for the given gates, keeping the rest in order.
func withoutGates(approvals []state.Approval, drop []domain.Gate) []state.Approval {
	kept := approvals[:0:0]
	for _, a := range approvals {
		keep := true
		for _, g := range drop {
			if a.Gate == g {
				keep = false
			}
		}
		if keep {
			kept = append(kept, a)
		}
	}
	return kept
}

// planGateRequired reports whether any service requires the plan approval.
func planGateRequired(m *Manager, l loaded) bool {
	for _, r := range gates.Requirements(m.opts.Global, l.ticket, l.state) {
		if r.Gate == domain.GatePlanApproval {
			return true
		}
	}
	return false
}
