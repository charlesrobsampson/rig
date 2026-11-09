package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/manualtests"
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
