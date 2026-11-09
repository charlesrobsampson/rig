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
	return l, release, nil
}
