package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/contract"
	"github.com/charlesrobsampson/rig/internal/fsutil"
	"github.com/charlesrobsampson/rig/internal/state"
)

// ContractList returns the ticket's contract proposals and any problems with the file.
func (m *Manager) ContractList(_ context.Context, p api.ContractListParams) (api.ContractListResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.ContractListResult{}, err
	}
	defer unlock()
	raw, err := os.ReadFile(filepath.Join(l.dir, "contract.md"))
	if err != nil {
		return api.ContractListResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no contract.md (only multi-service tickets have one)", p.Ticket)}
	}
	doc := contract.Parse(string(raw))
	res := api.ContractListResult{Ticket: p.Ticket, Proposals: []api.ContractProposal{}, Violation: l.state.Contract.Violation}
	for _, pr := range doc.Proposals {
		res.Proposals = append(res.Proposals, api.ContractProposal{ID: pr.ID, From: pr.From, Status: string(pr.Status), What: pr.What, Why: pr.Why, Rejected: pr.Rejected})
	}
	for _, pb := range doc.Problems {
		res.Problems = append(res.Problems, fmt.Sprintf("line %d: %s", pb.Line, pb.Message))
	}
	return res, nil
}

// ContractAccept merges an open proposal into the agreed section.
func (m *Manager) ContractAccept(ctx context.Context, p api.ContractResolveParams) (api.ContractResolveResult, error) {
	return m.resolveProposal(ctx, p, true)
}

// ContractReject records why an open proposal was turned down.
func (m *Manager) ContractReject(ctx context.Context, p api.ContractResolveParams) (api.ContractResolveResult, error) {
	return m.resolveProposal(ctx, p, false)
}

func (m *Manager) resolveProposal(_ context.Context, p api.ContractResolveParams, accept bool) (api.ContractResolveResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.ContractResolveResult{}, err
	}
	defer unlock()
	path := filepath.Join(l.dir, "contract.md")
	var resolved contract.Proposal
	outcome := "rejected"
	if accept {
		outcome = "accepted"
	}
	// The file is rewritten inside the state update: if the write fails nothing in
	// state.json changes, and the watcher never sees a half-applied resolution.
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		raw, err := os.ReadFile(path)
		if err != nil {
			return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no contract.md", p.Ticket)}
		}
		var out string
		if accept {
			out, err = contract.Accept(string(raw), p.Proposal, m.opts.Clock.Now())
		} else {
			out, err = contract.Reject(string(raw), p.Proposal, p.Reason, m.opts.Clock.Now())
		}
		switch {
		case errors.Is(err, contract.ErrNoProposal):
			return &api.Error{Code: api.CodeNotFound, Message: err.Error()}
		case errors.Is(err, contract.ErrNotOpen):
			return &api.Error{Code: api.CodeConflict, Message: err.Error()}
		case err != nil:
			return invalid("%v", err)
		}
		before := contract.Parse(string(raw))
		resolved, _ = before.Proposal(p.Proposal)
		if err := fsutil.AtomicWrite(path, []byte(out)); err != nil {
			return &api.Error{Code: api.CodeInternal, Message: err.Error()}
		}
		after := contract.Parse(out)
		s.Contract.AgreedHash = after.AgreedHash()
		s.Contract.Pending = len(after.OpenProposals()) > 0
		if !slices.Contains(s.Contract.Seen, p.Proposal) {
			s.Contract.Seen = append(s.Contract.Seen, p.Proposal)
		}
		if !slices.Contains(s.Contract.Resolved, p.Proposal) {
			s.Contract.Resolved = append(s.Contract.Resolved, p.Proposal)
		}
		return nil
	})
	if err != nil {
		return api.ContractResolveResult{}, err
	}
	m.publish(api.EventContractResolved, p.Ticket, "", api.ContractResolvedData{Proposal: p.Proposal, Outcome: outcome})
	if m.opts.OnContractResolved != nil {
		m.opts.OnContractResolved(p.Ticket, resolved, outcome, strings.TrimSpace(p.Reason))
	}
	return api.ContractResolveResult{Proposal: p.Proposal, Outcome: outcome}, nil
}

// ContractAck clears the direct-edit violation after a human has looked at it. The
// edited text becomes the agreed baseline, which the watcher already did when it saw
// the edit; acknowledging only clears the flag.
func (m *Manager) ContractAck(_ context.Context, p api.ContractAckParams) error {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return err
	}
	defer unlock()
	if l.state.Contract.Violation == "" {
		return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("ticket %q has no contract violation to acknowledge", p.Ticket)}
	}
	return m.opts.States.Update(l.statePath, func(s *state.State) error {
		s.Contract.Violation = ""
		return nil
	})
}

// ContractPropose appends an open proposal from a service to the ticket's contract.md.
// The contract watcher then announces it and tells the other services, exactly as for a
// proposal an agent wrote by hand.
func (m *Manager) ContractPropose(_ context.Context, p api.ContractProposeParams) (api.ContractProposeResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.ContractProposeResult{}, err
	}
	defer unlock()
	if !hasService(l.ticket, p.Service) {
		return api.ContractProposeResult{}, invalid("ticket %q has no service %q", p.Ticket, p.Service)
	}
	path := filepath.Join(l.dir, "contract.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return api.ContractProposeResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no contract.md (only multi-service tickets have one)", p.Ticket)}
	}
	id, out, err := contract.Propose(string(raw), p.Service, p.What, p.Why)
	if err != nil {
		return api.ContractProposeResult{}, invalid("%v", err)
	}
	if err := fsutil.AtomicWrite(path, []byte(out)); err != nil {
		return api.ContractProposeResult{}, &api.Error{Code: api.CodeInternal, Message: err.Error()}
	}
	return api.ContractProposeResult{Proposal: id}, nil
}
