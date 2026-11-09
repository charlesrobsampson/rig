// Package registry reads tickets from the tickets directory. Each call reads the
// files fresh, so a ticket created or edited by hand shows up immediately and the
// files stay the single source of truth.
package registry

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

var ErrNotFound = errors.New("ticket not found")

type Registry struct {
	dir    string
	states *state.Manager
}

func New(ticketsDir string, states *state.Manager) *Registry {
	return &Registry{dir: ticketsDir, states: states}
}

// List summarizes every ticket directory in id order. A ticket whose files cannot be
// read is still listed, with its problem in Error, rather than hidden.
func (r *Registry) List() ([]api.TicketSummary, error) {
	entries, err := os.ReadDir(r.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []api.TicketSummary{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tickets directory: %w", err)
	}
	tickets := []api.TicketSummary{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			tickets = append(tickets, r.load(e.Name()).Summary)
		}
	}
	return tickets, nil
}

// Get returns whatever could be read of one ticket; Summary.Error says what could not.
func (r *Registry) Get(id string) (api.TicketGetResult, error) {
	if err := domain.ValidateTicketID(id); err != nil {
		return api.TicketGetResult{}, err
	}
	if info, err := os.Stat(filepath.Join(r.dir, id)); err != nil || !info.IsDir() {
		return api.TicketGetResult{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return r.load(id), nil
}

func (r *Registry) load(id string) api.TicketGetResult {
	dir := filepath.Join(r.dir, id)
	res := api.TicketGetResult{Summary: api.TicketSummary{ID: id, Services: []api.ServiceSummary{}}}
	var problems []string

	if err := domain.ValidateTicketID(id); err != nil {
		problems = append(problems, "directory name is not a valid ticket id")
	}

	// Listing never checks that repositories exist; starting a ticket does.
	ticket, err := config.LoadTicket(filepath.Join(dir, "ticket.yaml"), config.TicketOptions{RepoCheck: func(string) error { return nil }})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		problems = append(problems, "ticket.yaml not found")
	case err != nil:
		msg := strings.TrimPrefix(strings.TrimPrefix(oneLine(err), "invalid ticket.yaml: "), "parse ticket.yaml: ")
		problems = append(problems, "ticket.yaml: "+msg)
	case ticket.ID != id:
		problems = append(problems, fmt.Sprintf("ticket.yaml id %q does not match its directory %q", ticket.ID, id))
	default:
		res.Ticket = &ticket
		res.Summary.Title = ticket.Title
	}

	st, err := r.states.Load(filepath.Join(dir, "state.json"))
	switch {
	case errors.Is(err, state.ErrNotFound):
		problems = append(problems, "state.json not found")
	case err != nil:
		problems = append(problems, "state.json: "+oneLine(err))
	default:
		res.State = &st
		res.Summary.Phase = st.Phase
		res.Summary.ContractPending = st.Contract.Pending
		res.Summary.ContractViolation = st.Contract.Violation != ""
	}

	if res.Ticket != nil {
		for _, svc := range res.Ticket.Services {
			summary := api.ServiceSummary{Name: svc.Name}
			if ss, ok := st.Services[svc.Name]; res.State != nil && ok {
				summary.Status, summary.AgentState, summary.HostPort = ss.Status, ss.Agent.State, ss.HostPort
			}
			res.Summary.Services = append(res.Summary.Services, summary)
		}
	}
	res.Summary.Error = strings.Join(problems, "; ")
	return res
}

// oneLine folds a validation error's bullet list into a single line for a table cell.
func oneLine(err error) string {
	msg := err.Error()
	if head, rest, ok := strings.Cut(msg, ":\n  - "); ok {
		msg = head + ": " + strings.ReplaceAll(rest, "\n  - ", "; ")
	}
	return strings.ReplaceAll(msg, "\n", " ")
}
