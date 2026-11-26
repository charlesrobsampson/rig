package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/gates"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/harness"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/timeline"
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
		if e := grantable(l.ticket, *s, service, to); e != nil {
			return e
		}
		s.PhaseRequest = &state.PhaseRequest{To: to, Service: service, Reason: reason, At: m.opts.Clock.Now().UTC()}
		return nil
	})
	if err != nil {
		// The agent would otherwise wait for a move that can never come.
		m.deliver(id, service, fmt.Sprintf("Your request for %s was not recorded: %v", to, err))
		return err
	}
	m.publish(api.EventPhaseRequested, id, service, api.PhaseRequestedData{To: to, Reason: reason})
	m.note(l.dir, timeline.PhaseRequested, "agent:"+service, service, timeline.PhaseRequestedData{To: string(to), Reason: reason})
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
	var pinRefusal string
	if l.ticket.Review != nil && l.state.Phase == domain.PhaseImplement && !state.IsBackward(l.state.Phase, to) {
		pinRefusal = m.reviewPin(context.Background(), l.id, l.ticket, l.state)
	}
	var made state.Transition
	var endedLoop *state.Review
	err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		// Decided against the state as it is now, inside the update, so the check and the
		// write cannot be separated by another move.
		if e := m.decide(id, l.ticket, *s, to, reason); e != nil {
			return e
		}
		if pinRefusal != "" && s.Review != nil && s.Review.Status == domain.ReviewPassed {
			return &api.Error{Code: api.CodeBlocked, Message: pinRefusal}
		}
		made = state.Transition{From: s.Phase, To: to, By: by, Reason: reason, At: m.opts.Clock.Now().UTC()}
		if state.IsBackward(s.Phase, to) {
			s.Approvals = withoutGates(s.Approvals, gates.InvalidatedByMovingTo(to))
		}
		endedLoop = nil
		if to == domain.PhaseImplement && state.IsBackward(s.Phase, to) {
			if s.Review != nil {
				ended := *s.Review
				endedLoop = &ended
			}
			s.Review = nil // work sent back is reviewed again, in a new loop
		}
		s.Phase = to
		s.PhaseRequest = nil
		s.SyncReturn = ""
		s.Transitions = append(s.Transitions, made)
		return nil
	})
	if err != nil {
		if _, ok := err.(*api.Error); ok {
			return api.TransitionResult{}, err
		}
		return api.TransitionResult{}, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record transition: %v", err)}
	}

	m.announceMove(l, made, service)
	if endedLoop != nil {
		m.note(l.dir, timeline.ReviewLoopEnded, by, "", timeline.ReviewLoopEndedData{Rounds: len(endedLoop.Rounds), Status: string(endedLoop.Status)})
	}
	res, err := m.reg.Get(id)
	if err != nil {
		return api.TransitionResult{}, err
	}
	return api.TransitionResult{Summary: res.Summary, From: made.From, To: made.To, By: by, Reason: reason, At: made.At}, nil
}

// announceMove tells everyone who follows the ticket about a recorded move.
func (m *Manager) announceMove(l loaded, made state.Transition, service string) {
	m.publish(api.EventPhaseChanged, l.id, "", api.PhaseChangedData{From: made.From, To: made.To, By: made.By, Reason: made.Reason})
	m.note(l.dir, timeline.PhaseMoved, made.By, service, timeline.PhaseMovedData{From: string(made.From), To: string(made.To), Reason: made.Reason})
	if m.opts.OnPhaseChanged != nil {
		if notices := phaseNotices(l, made); len(notices) > 0 {
			m.opts.OnPhaseChanged(l.id, notices)
		}
	}
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
	if t.Review != nil && from == domain.PhaseImplement && (st.Review == nil || st.Review.Status != domain.ReviewPassed) {
		status := "no review round has run"
		if st.Review != nil {
			status = fmt.Sprintf("the review is %s (round %d)", st.Review.Status, st.Review.Round)
			if st.Review.Reason != "" {
				status += ": " + st.Review.Reason
			}
		}
		return &api.Error{Code: api.CodeBlocked, Message: fmt.Sprintf("ticket %q leaves implement only after its review passes; %s. A person can decide with: rig reviewer %s --pass --reason \"...\"", id, status, id)}
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

// phaseNotices tell each running agent about a move, with the ticket files named where
// that agent sees them.
func phaseNotices(l loaded, made state.Transition) map[string]string {
	notices := map[string]string{}
	for _, svc := range l.ticket.Services {
		agent := l.state.Services[svc.Name].Agent
		if agent.PaneID == "" || agent.State == domain.AgentExited {
			continue
		}
		req := harness.Request{Ticket: l.ticket, Service: svc.Name, Phase: made.To, TicketDir: l.dir, Reviewed: l.ticket.Review != nil && svc.Name == l.ticket.Review.Of}
		if agent.ContainerID != "" {
			req.AgentTicketDir = agentTicketDir
		}
		notices[svc.Name] = harness.PhaseNotice(req, made.From, made.Reason)
	}
	return notices
}

// grantable refuses a request a human could never grant as asked: a skipped phase, the
// phase the ticket is in, or a reviewed coder leaving implement, which goes through the
// review loop instead. It says what to ask for.
func grantable(t domain.Ticket, st state.State, service string, to domain.Phase) *api.Error {
	if err := state.CanTransition(st.Phase, to); err != nil {
		msg := err.Error()
		if next, ok := state.NextPhase(st.Phase); ok && !state.IsBackward(st.Phase, to) && st.Phase != to {
			msg += "; ask for " + string(next)
		}
		return invalid("%s", msg)
	}
	if t.Review != nil && service == t.Review.Of && st.Phase == domain.PhaseImplement && !state.IsBackward(st.Phase, to) &&
		(st.Review == nil || st.Review.Status != domain.ReviewPassed) {
		return invalid("a reviewer checks this work first: leave a review.ready record instead of asking for %s", to)
	}
	return nil
}
