package lifecycle

import (
	"context"
	"fmt"
	"github.com/charlesrobsampson/rig/internal/contract"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/gates"
	"github.com/charlesrobsampson/rig/internal/state"
)

// Gates lists every approval the ticket's services require, and which are given.
func (m *Manager) Gates(_ context.Context, p api.GateListParams) (api.GateListResult, error) {
	l, unlock, err := m.begin(p.ID)
	if err != nil {
		return api.GateListResult{}, err
	}
	defer unlock()
	return m.gateList(l.ticket, l.state), nil
}

func (m *Manager) gateList(t domain.Ticket, st state.State) api.GateListResult {
	res := api.GateListResult{Phase: st.Phase, Gates: []api.GateStatus{}}
	current, hasCurrent := gates.GuardingExit(st.Phase)
	for _, r := range gates.Requirements(m.opts.Global, t, st) {
		from, to, _ := gates.Transition(r.Gate)
		gs := api.GateStatus{
			Gate: r.Gate, From: from, To: to, Service: r.Service,
			Approved: r.Approved, By: r.By,
			Current: hasCurrent && r.Gate == current,
		}
		if r.Approved {
			at := r.At
			gs.At = &at
		}
		res.Gates = append(res.Gates, gs)
	}
	return res
}

// Approve records a human approval of a gate. The gate must be required by the
// service it names (or by some service, for a blanket approval) and must be the one
// guarding the move out of the ticket's current phase. Approving what is already
// approved changes nothing. Only humans approve: the API method behind this is never
// offered to agents.
func (m *Manager) Approve(_ context.Context, p api.GateApproveParams) (api.GateListResult, error) {
	if p.Gate == "" {
		return api.GateListResult{}, invalid("a gate is required (one of %s, %s, %s)", domain.GatePlanApproval, domain.GateManualTestSignoff, domain.GateReviewApproval)
	}
	l, unlock, err := m.begin(p.ID)
	if err != nil {
		return api.GateListResult{}, err
	}
	defer unlock()

	if l.state.Phase == domain.PhaseDone {
		return api.GateListResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("ticket %q is done", p.ID)}
	}
	var requiring []string
	for _, r := range gates.Requirements(m.opts.Global, l.ticket, l.state) {
		if r.Gate == p.Gate {
			requiring = append(requiring, r.Service)
		}
	}
	if len(requiring) == 0 {
		return api.GateListResult{}, invalid("gate %s is not required by any service of ticket %q", p.Gate, p.ID)
	}
	if p.Service != "" && !contains(requiring, p.Service) {
		return api.GateListResult{}, invalid("service %q does not require gate %s (services that do: %s)", p.Service, p.Gate, strings.Join(requiring, ", "))
	}
	if current, ok := gates.GuardingExit(l.state.Phase); !ok || current != p.Gate {
		from, to, _ := gates.Transition(p.Gate)
		hint := fmt.Sprintf("phase %s has no gate", l.state.Phase)
		if ok {
			hint = fmt.Sprintf("approve %s to leave %s", current, l.state.Phase)
		}
		return api.GateListResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf(
			"gate %s guards %s -> %s, but ticket %q is in phase %s; %s", p.Gate, from, to, p.ID, l.state.Phase, hint)}
	}

	if p.Gate == domain.GatePlanApproval {
		if e := m.contractAgreed(l, "approve the plan"); e != nil {
			return api.GateListResult{}, e
		}
	}

	added := false
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		added = false
		if m.covered(l.ticket, *s, p) {
			return nil
		}
		s.Approvals = append(s.Approvals, state.Approval{Gate: p.Gate, Service: p.Service, By: "human", At: m.opts.Clock.Now().UTC()})
		added = true
		return nil
	})
	if err != nil {
		return api.GateListResult{}, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record approval: %v", err)}
	}
	if added {
		m.publish(api.EventGateApproved, p.ID, "", api.GateApprovedData{Gate: p.Gate, Service: p.Service})
	}

	st, err := m.opts.States.Load(l.statePath)
	if err != nil {
		return api.GateListResult{}, &api.Error{Code: api.CodeInternal, Message: err.Error()}
	}
	return m.gateList(l.ticket, st), nil
}

// covered reports whether the approval asked for is already fully in place.
func (m *Manager) covered(t domain.Ticket, st state.State, p api.GateApproveParams) bool {
	for _, r := range gates.Requirements(m.opts.Global, t, st) {
		if r.Gate == p.Gate && (p.Service == "" || r.Service == p.Service) && !r.Approved {
			return false
		}
	}
	return true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// contractAgreed refuses, for a multi-service ticket, while its contract's agreed
// section is empty: the services cannot build against each other without one. A
// single-service ticket has no contract to agree. what names the action refused.
func (m *Manager) contractAgreed(l loaded, what string) *api.Error {
	if len(l.ticket.Services) < 2 {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(l.dir, "contract.md"))
	if err == nil && !contract.Parse(string(raw)).AgreedEmpty() {
		return nil
	}
	detail := "contract.md has nothing under \"## Agreed\""
	if err != nil {
		detail = "contract.md is missing"
	}
	return &api.Error{
		Code:    api.CodeBlocked,
		Message: fmt.Sprintf("cannot %s for ticket %q: the services have not agreed a contract yet (%s)", what, l.id, detail),
		Details: []string{"contract: write what the services agree on under \"## Agreed\" in " + filepath.Join(l.dir, "contract.md")},
	}
}
