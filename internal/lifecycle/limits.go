package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/state"
)

// reserveSlot lets a start through the limits.max_active_tickets cap, or refuses it. A
// ticket that is already active (a start repeated to retry an agent, say) always passes:
// it is already counted, and refusing it would strand work. Otherwise the ticket needs a
// free slot among the other active tickets and the starts already let through. The slot
// is held until release, which covers the gap before state.json records the start.
func (m *Manager) reserveSlot(id string, st state.State) (release func(), err error) {
	limit := m.opts.Global.Limits.MaxActiveTickets
	if limit <= 0 || st.Active() {
		return func() {}, nil
	}
	m.capMu.Lock()
	defer m.capMu.Unlock()

	taken := map[string]bool{}
	for _, other := range m.activeTickets(id) {
		taken[other] = true
	}
	for other := range m.starting {
		if other != id {
			taken[other] = true
		}
	}
	if len(taken) >= limit {
		names := make([]string, 0, len(taken))
		for n := range taken {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, &api.Error{
			Code:    api.CodeBlocked,
			Message: fmt.Sprintf("cannot start ticket %q: %d tickets are already active and the limit is %d (limits.max_active_tickets); stop one first with rig stop <id>", id, len(taken), limit),
			Details: []string{"active: " + strings.Join(names, ", ")},
		}
	}
	m.starting[id] = true
	return func() {
		m.capMu.Lock()
		delete(m.starting, id)
		m.capMu.Unlock()
	}, nil
}

// activeTickets lists the other tickets whose state says they are active.
func (m *Manager) activeTickets(except string) []string {
	entries, err := os.ReadDir(m.opts.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == except || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		st, err := m.opts.States.Load(filepath.Join(m.opts.Dir, e.Name(), "state.json"))
		if err == nil && st.Active() {
			out = append(out, e.Name())
		}
	}
	return out
}
