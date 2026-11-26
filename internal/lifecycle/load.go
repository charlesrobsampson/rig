package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

// loaded is a ticket as read from disk, held under its lock.
type loaded struct {
	id        string
	dir       string
	statePath string
	ticket    domain.Ticket
	state     state.State
}

// begin validates the id, takes the ticket's lock, and reads ticket.yaml and
// state.json. Callers must call unlock when done (it is nil when err is set).
func (m *Manager) begin(id string) (l loaded, unlock func(), err error) {
	if err := domain.ValidateTicketID(id); err != nil {
		return loaded{}, nil, invalid("%v", err)
	}
	release := m.lock(id)
	defer func() {
		if err != nil {
			release() // a failed begin must not leave the ticket locked
		}
	}()

	l = loaded{id: id, dir: filepath.Join(m.opts.Dir, id)}
	if info, err := os.Stat(l.dir); err != nil || !info.IsDir() {
		return loaded{}, nil, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q not found", id)}
	}
	// Repositories are checked where they matter (start), not for every operation.
	l.ticket, err = config.LoadTicket(filepath.Join(l.dir, "ticket.yaml"), config.TicketOptions{RepoCheck: func(string) error { return nil }})
	if err != nil {
		return loaded{}, nil, invalid("%v", err)
	}
	l.statePath = filepath.Join(l.dir, "state.json")
	if l.state, err = m.opts.States.Load(l.statePath); err != nil {
		return loaded{}, nil, invalid("%v", err)
	}
	if err := m.settleLostRound(&l); err != nil {
		return loaded{}, nil, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("a review round was lost and could not be marked awaiting_ready: %v", err)}
	}
	return l, release, nil
}

// settleLostRound turns a round that is marked reviewing but has no commit and is not
// being started by this process into awaiting_ready, in the copy read and in
// state.json: nothing runs it, so review.ready or --start must be able to start it.
func (m *Manager) settleLostRound(l *loaded) error {
	r := l.state.Review
	if r == nil || r.Status != domain.ReviewReviewing || r.Commit != "" || m.roundPending(l.id) {
		return nil
	}
	err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		if s.Review != nil && s.Review.Status == domain.ReviewReviewing && s.Review.Commit == "" {
			s.Review.Status = domain.ReviewAwaitingReady
		}
		return nil
	})
	if err != nil {
		return err
	}
	cp := *r
	cp.Status = domain.ReviewAwaitingReady
	l.state.Review = &cp
	return nil
}

// RoundPending says whether a review round is being started for the ticket.
func (m *Manager) RoundPending(id string) bool { return m.roundPending(id) }

func (m *Manager) roundPending(id string) bool {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return m.pending[id]
}

func (m *Manager) setPending(id string, on bool) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	if on {
		m.pending[id] = true
	} else {
		delete(m.pending, id)
	}
}

// load reads a ticket as begin does, for a caller that only reads it.
func (m *Manager) load(id string) (loaded, error) {
	l, unlock, err := m.begin(id)
	if err != nil {
		return loaded{}, err
	}
	unlock()
	return l, nil
}
