package lifecycle

import "context"

// BeginRound, RunRound and MarkPending let tests drive the two halves of a round apart.
func (m *Manager) BeginRound(id, by string) error          { return m.beginRound(id, by) }
func (m *Manager) RunRound(ctx context.Context, id string) { m.runRound(ctx, id) }

// Hold takes a ticket's lifecycle lock as a start or stop under way does.
func (m *Manager) Hold(id string) func() { return m.lock(id) }
