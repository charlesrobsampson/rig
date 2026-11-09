package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/manualtests"
	"github.com/charlesrobsampson/rig/internal/state"
)

// RenderTests reads the ticket's manual-tests.md and fills its placeholders from the
// live state of the ticket (host ports of running services, internal addresses,
// dependency hostnames). A placeholder it cannot fill is an error naming every one, so
// the agent can fix the file in one pass.
func (m *Manager) RenderTests(_ context.Context, p api.TestsRenderParams) (api.TestsRenderResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.TestsRenderResult{}, err
	}
	defer unlock()

	raw, err := os.ReadFile(filepath.Join(l.dir, "manual-tests.md"))
	if errors.Is(err, os.ErrNotExist) {
		return api.TestsRenderResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no manual-tests.md yet; the agent writes it during the test phase", p.Ticket)}
	}
	if err != nil {
		return api.TestsRenderResult{}, &api.Error{Code: api.CodeInternal, Message: err.Error()}
	}

	vars := manualtests.Vars{TicketID: l.id, Services: map[string]manualtests.ServiceVars{}, Dependencies: map[string]bool{}}
	for _, svc := range l.ticket.Services {
		rec := l.state.Services[svc.Name]
		sv := manualtests.ServiceVars{InternalPort: svc.InternalPort}
		if rec.HostPort != 0 && rec.Status == "running" {
			sv.HostPort = rec.HostPort
		} else {
			sv.NotRunning = string(rec.Status)
			if sv.NotRunning == "" {
				sv.NotRunning = "not started"
			}
		}
		vars.Services[svc.Name] = sv
	}
	for _, dep := range l.ticket.Dependencies {
		vars.Dependencies[dep.Name] = true
	}

	doc, err := manualtests.Render(string(raw), vars)
	var problems *manualtests.Problems
	if errors.As(err, &problems) {
		return api.TestsRenderResult{}, &api.Error{Code: api.CodeInvalid, Message: "manual-tests.md cannot be rendered", Details: problems.Problems}
	}
	if err != nil {
		return api.TestsRenderResult{}, err
	}

	res := api.TestsRenderResult{Preamble: doc.Preamble, Steps: []api.TestsStep{}, SignedOff: l.state.ManualTests.SignedOff}
	for _, s := range doc.Steps {
		res.Steps = append(res.Steps, api.TestsStep{ID: s.ID, Title: s.Title, Body: s.Body, Done: l.state.ManualTests.Steps[s.ID]})
	}
	return res, nil
}

// manualSteps reads the step ids from the ticket's manual-tests.md.
func manualSteps(dir, id string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manual-tests.md"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no manual-tests.md yet; the agent writes it during the test phase", id)}
	}
	if err != nil {
		return nil, &api.Error{Code: api.CodeInternal, Message: err.Error()}
	}
	doc, err := manualtests.Parse(string(raw))
	if err != nil {
		var problems *manualtests.Problems
		if errors.As(err, &problems) {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "manual-tests.md has malformed steps", Details: problems.Problems}
		}
		return nil, err
	}
	ids := make([]string, len(doc.Steps))
	for i, s := range doc.Steps {
		ids[i] = s.ID
	}
	return ids, nil
}

// TestsStep ticks a manual-test step on or off. Unticking a step of a signed-off ticket
// withdraws the sign-off and its approval: the result of the pass no longer holds.
func (m *Manager) TestsStep(_ context.Context, p api.TestsStepParams) (api.TestsStepResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.TestsStepResult{}, err
	}
	defer unlock()
	steps, err := manualSteps(l.dir, l.id)
	if err != nil {
		return api.TestsStepResult{}, err
	}
	if !contains(steps, p.Step) {
		return api.TestsStepResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("manual-tests.md has no step %q (steps: %s)", p.Step, strings.Join(steps, ", "))}
	}
	var signedOff bool
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		if s.ManualTests.Steps == nil {
			s.ManualTests.Steps = map[string]bool{}
		}
		if p.Done {
			s.ManualTests.Steps[p.Step] = true
		} else {
			delete(s.ManualTests.Steps, p.Step)
			if s.ManualTests.SignedOff {
				s.ManualTests.SignedOff = false
				s.Approvals = withoutGates(s.Approvals, []domain.Gate{domain.GateManualTestSignoff})
			}
		}
		signedOff = s.ManualTests.SignedOff
		return nil
	})
	if err != nil {
		return api.TestsStepResult{}, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record the step: %v", err)}
	}
	return api.TestsStepResult{Step: p.Step, Done: p.Done, SignedOff: signedOff}, nil
}

// TestsSignoff records the human's sign-off of the manual tests, and approves the
// manual_test_signoff gate when some service requires it. It needs the test phase (where
// the gate stands) and every step ticked, unless the human overrides.
func (m *Manager) TestsSignoff(_ context.Context, p api.TestsSignoffParams) (api.TestsSignoffResult, error) {
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.TestsSignoffResult{}, err
	}
	defer unlock()
	if l.state.Phase != domain.PhaseTest {
		return api.TestsSignoffResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("manual tests are signed off in the test phase, but ticket %q is in %s", p.Ticket, l.state.Phase)}
	}
	steps, err := manualSteps(l.dir, l.id)
	if err != nil {
		return api.TestsSignoffResult{}, err
	}
	var unchecked []string
	for _, id := range steps {
		if !l.state.ManualTests.Steps[id] {
			unchecked = append(unchecked, id)
		}
	}
	if len(steps) == 0 && !p.Override {
		return api.TestsSignoffResult{}, &api.Error{Code: api.CodeBlocked, Message: "manual-tests.md has no steps to sign off", Details: []string{"override to sign off with nothing tested"}}
	}
	if len(unchecked) > 0 && !p.Override {
		details := make([]string, len(unchecked))
		for i, id := range unchecked {
			details[i] = "step not done: " + id
		}
		return api.TestsSignoffResult{}, &api.Error{Code: api.CodeBlocked, Message: fmt.Sprintf("%d of %d manual-test steps are not done", len(unchecked), len(steps)), Details: details}
	}

	var res api.TestsSignoffResult
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		res = api.TestsSignoffResult{SignedOff: true, Unchecked: unchecked}
		s.ManualTests.SignedOff = true
		gate := api.GateApproveParams{ID: l.id, Gate: domain.GateManualTestSignoff}
		if !m.covered(l.ticket, *s, gate) {
			s.Approvals = append(s.Approvals, state.Approval{Gate: gate.Gate, By: "human", At: m.opts.Clock.Now().UTC()})
			res.Approved = true
		}
		return nil
	})
	if err != nil {
		return api.TestsSignoffResult{}, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("record the sign-off: %v", err)}
	}
	if res.Approved {
		m.publish(api.EventGateApproved, l.id, "", api.GateApprovedData{Gate: domain.GateManualTestSignoff})
	}
	return res, nil
}
