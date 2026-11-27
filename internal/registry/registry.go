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
	"time"

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
	// base, when set, says how each service's branch stands against its base.
	base func(domain.Ticket, state.State) []api.BaseInfo
	// touched, when set, lists the files each service has changed against its base.
	touched func(domain.Ticket, state.State) []api.TouchedInfo
	// aiSummary, when set, supplies a ticket's language-model summary.
	aiSummary func(id string) *api.SummaryInfo
	// drift, when set, supplies how a ticket's reality differs from its state.
	drift func(id string) []string
	// roundPending, when set, says whether a review round is being started for a ticket.
	roundPending func(id string) bool
	// reviewStale, when set, says whether a passed review is of a commit the branch has left.
	reviewStale func(domain.Ticket, state.State) bool
	// now is the clock for a stalled round (nil means the real time).
	now func() time.Time
}

// WithClock sets the time a round counts as stalled against.
func (r *Registry) WithClock(now func() time.Time) *Registry {
	r.now = now
	return r
}

// stalledChecker returns the checker a reviewing round waits on whose agent has been idle
// for api.StalledAfter, if any.
func (r *Registry) stalledChecker(t *domain.Ticket, st state.State, rv *state.Review) *api.ReviewStalled {
	var round state.ReviewRound
	for _, rr := range rv.Rounds {
		if rr.Round == rv.Round {
			round = rr
		}
	}
	owing := []string{}
	if !round.ReviewerDone {
		owing = append(owing, domain.ReviewerService)
	}
	if t != nil && t.Review != nil && t.Review.Tester != nil && round.Tester == nil {
		owing = append(owing, domain.TesterService)
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	for _, name := range owing {
		ag := st.Services[name].Agent
		if ag.State == domain.AgentIdle && !ag.IdleSince.IsZero() && now.Sub(ag.IdleSince) >= api.StalledAfter {
			return &api.ReviewStalled{Service: name, Since: ag.IdleSince}
		}
	}
	return nil
}

// WithReviewStale makes summaries mark a passed review whose commit the branch has moved past.
func (r *Registry) WithReviewStale(f func(domain.Ticket, state.State) bool) *Registry {
	r.reviewStale = f
	return r
}

// WithRoundPending makes summaries tell a round that is only starting (reviewing, no
// commit yet) from one nothing runs: the latter is shown as awaiting_ready.
func (r *Registry) WithRoundPending(f func(id string) bool) *Registry {
	r.roundPending = f
	return r
}

// WithBase makes summaries carry how each service's branch stands against its base.
func (r *Registry) WithBase(f func(domain.Ticket, state.State) []api.BaseInfo) *Registry {
	r.base = f
	return r
}

// WithTouched makes summaries carry the files each service has changed.
func (r *Registry) WithTouched(f func(domain.Ticket, state.State) []api.TouchedInfo) *Registry {
	r.touched = f
	return r
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
		if !st.CreatedAt.IsZero() {
			created := st.CreatedAt
			res.Summary.CreatedAt = &created
		}
		for _, tr := range st.Transitions {
			if tr.To == domain.PhaseDone && st.Phase == domain.PhaseDone {
				done := tr.At
				res.Summary.DoneAt = &done
			}
		}
		for _, ss := range st.Services { // the reviewer is a service in state, so it counts
			if ss.Agent.Cost != nil {
				total := ss.Agent.Cost.USD
				if res.Summary.CostUSD != nil {
					total += *res.Summary.CostUSD
				}
				res.Summary.CostUSD = &total
			}
		}
		if rv := st.Review; rv != nil {
			status := rv.Status
			if status == domain.ReviewReviewing && rv.Commit == "" && (r.roundPending == nil || !r.roundPending(id)) {
				status = domain.ReviewAwaitingReady
			}
			res.Summary.Review = &api.ReviewSummary{Status: status, Round: rv.Round, Reason: rv.Reason, At: rv.At}
			if status == domain.ReviewReviewing && rv.Commit != "" {
				res.Summary.Review.Stalled = r.stalledChecker(res.Ticket, st, rv)
			}
			if rv.Status == domain.ReviewPassed && rv.Commit != "" && r.reviewStale != nil && res.Ticket != nil {
				res.Summary.Review.Stale = r.reviewStale(*res.Ticket, st)
			}
		}
		if q := st.Question; q != nil {
			res.Summary.Question = &api.QuestionSummary{Service: q.Service, Text: q.Text, At: q.At}
		}
		if r := st.PhaseRequest; r != nil {
			res.Summary.PhaseRequest = &api.PhaseRequestSummary{To: r.To, Service: r.Service, Reason: r.Reason, At: r.At}
		}
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
				if ss.Agent.State == domain.AgentIdle && !ss.Agent.IdleSince.IsZero() {
					since := ss.Agent.IdleSince
					summary.IdleSince = &since
				}
			}
			res.Summary.Services = append(res.Summary.Services, summary)
		}
	}
	if r.aiSummary != nil {
		res.Summary.AISummary = r.aiSummary(id)
	}
	if r.base != nil && res.Ticket != nil && res.State != nil {
		res.Summary.Base = r.base(*res.Ticket, *res.State)
	}
	if r.touched != nil && res.Ticket != nil && res.State != nil {
		res.Summary.Touched = r.touched(*res.Ticket, *res.State)
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
