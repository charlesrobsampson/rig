// Package gates decides which human approvals a ticket needs before it may change
// phase (docs/formats.md §6). It is pure: given the config, the ticket, and its
// recorded approvals, it says what is required and what is still missing.
package gates

import (
	"slices"
	"time"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/state"
)

type guard struct {
	gate     domain.Gate
	from, to domain.Phase
}

// guards lists the gated transitions in lifecycle order. Only forward moves are gated.
var guards = []guard{
	{domain.GatePlanApproval, domain.PhasePlan, domain.PhaseImplement},
	{domain.GateManualTestSignoff, domain.PhaseTest, domain.PhaseReview},
	{domain.GateReviewApproval, domain.PhaseReview, domain.PhasePush},
}

// GateFor is the gate guarding the move from one phase to another, if any.
func GateFor(from, to domain.Phase) (domain.Gate, bool) {
	for _, g := range guards {
		if g.from == from && g.to == to {
			return g.gate, true
		}
	}
	return "", false
}

// GuardingExit is the gate that must be satisfied to leave a phase going forward.
func GuardingExit(from domain.Phase) (domain.Gate, bool) {
	for _, g := range guards {
		if g.from == from {
			return g.gate, true
		}
	}
	return "", false
}

// Transition is the move a gate guards.
func Transition(gate domain.Gate) (from, to domain.Phase, ok bool) {
	for _, g := range guards {
		if g.gate == gate {
			return g.from, g.to, true
		}
	}
	return "", "", false
}

// Requirement is one gate one service must pass, and whether it has.
type Requirement struct {
	Gate     domain.Gate
	Service  string
	Approved bool
	By       string
	At       time.Time
}

// Requirements lists, in lifecycle then service order, every gate each service requires
// under its effective policy (service, then ticket, then global). A gate that no
// service requires does not appear, so it never blocks anything. An approval with an
// empty service covers every service.
func Requirements(g config.Global, t domain.Ticket, st state.State) []Requirement {
	var reqs []Requirement
	for _, guard := range guards {
		for _, svc := range t.Services {
			if !slices.Contains(policy.Resolve(g, t, svc, policy.Flags{}).Gates, guard.gate) {
				continue
			}
			req := Requirement{Gate: guard.gate, Service: svc.Name}
			for _, a := range st.Approvals { // the latest matching approval wins
				if a.Gate == guard.gate && (a.Service == "" || a.Service == svc.Name) {
					req.Approved, req.By, req.At = true, a.By, a.At
				}
			}
			reqs = append(reqs, req)
		}
	}
	return reqs
}

// Missing lists the services that still need to approve a gate.
func Missing(reqs []Requirement, gate domain.Gate) []string {
	var services []string
	for _, r := range reqs {
		if r.Gate == gate && !r.Approved {
			services = append(services, r.Service)
		}
	}
	return services
}

// InvalidatedByMovingTo lists the gates whose approvals no longer hold after the ticket
// moves back to a phase: the work they vouched for is being redone.
func InvalidatedByMovingTo(to domain.Phase) []domain.Gate {
	var gates []domain.Gate
	for _, g := range guards {
		if state.PhaseIndex(g.from) >= state.PhaseIndex(to) {
			gates = append(gates, g.gate)
		}
	}
	return gates
}

// Awaiting is the gate a human has yet to approve to move the ticket on: the gate
// guarding the exit of its current phase, when some service requires it and not every
// service has it. ok is false when nothing is waiting on a human.
func Awaiting(g config.Global, t domain.Ticket, st state.State) (gate domain.Gate, ok bool) {
	gate, guarded := GuardingExit(st.Phase)
	if !guarded {
		return "", false
	}
	if len(Missing(Requirements(g, t, st), gate)) == 0 {
		return "", false
	}
	return gate, true
}
