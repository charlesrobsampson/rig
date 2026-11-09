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
	"slices"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

var (
	ErrNotFound = errors.New("ticket not found")
	// ErrBadFile is a request for a file that is not one of the ticket's lifecycle files.
	ErrBadFile = errors.New("not a readable ticket file")
)

type Registry struct {
	dir    string
	states *state.Manager
	// pendingGate, when set, names the approval a ticket is waiting for (it needs the
	// global policy, which the registry does not know).
	pendingGate func(domain.Ticket, state.State) domain.Gate
	// aiSummary, when set, supplies a ticket's language-model summary.
	aiSummary func(id string) *api.SummaryInfo
	// drift, when set, supplies how a ticket's reality differs from its state.
	drift func(id string) []string
}

// WithDrift makes summaries carry the drift last found for each ticket.
func (r *Registry) WithDrift(f func(id string) []string) *Registry {
	r.drift = f
	return r
}

// WithAISummary makes summaries carry the ticket's language-model summary, if any.
func (r *Registry) WithAISummary(f func(id string) *api.SummaryInfo) *Registry {
	r.aiSummary = f
	return r
}

// WithPendingGate makes summaries carry the gate a human has yet to approve.
func (r *Registry) WithPendingGate(f func(domain.Ticket, state.State) domain.Gate) *Registry {
	r.pendingGate = f
	return r
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

// File reads one of the ticket's lifecycle files (api.TicketFileNames). It is not_found
// when the ticket or the file does not exist, and an error naming the allowed files for
// any other name, so a client can never read outside the ticket's files.
func (r *Registry) File(id, name string) (string, error) {
	if err := domain.ValidateTicketID(id); err != nil {
		return "", err
	}
	if !slices.Contains(api.TicketFileNames, name) {
		return "", fmt.Errorf("%w: %q (readable files: %s)", ErrBadFile, name, strings.Join(api.TicketFileNames, ", "))
	}
	if info, err := os.Stat(filepath.Join(r.dir, id)); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, id, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s has no %s", ErrNotFound, id, name)
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return string(raw), nil
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

	if res.Ticket != nil && res.State != nil && r.pendingGate != nil {
		res.Summary.PendingGate = r.pendingGate(*res.Ticket, *res.State)
	}
	if res.Ticket != nil {
		for _, svc := range res.Ticket.Services {
			summary := api.ServiceSummary{Name: svc.Name}
			if ss, ok := st.Services[svc.Name]; res.State != nil && ok {
				summary.Status, summary.AgentState, summary.HostPort = ss.Status, ss.Agent.State, ss.HostPort
				if ss.Agent.Cost != nil {
					usd := ss.Agent.Cost.USD
					summary.CostUSD = &usd
				}
			}
			res.Summary.Services = append(res.Summary.Services, summary)
		}
	}
	if r.aiSummary != nil {
		res.Summary.AISummary = r.aiSummary(id)
	}
	if r.drift != nil {
		res.Summary.Drift = r.drift(id)
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
