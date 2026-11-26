package lifecycle

import "context"

// BeginRound, RunRound and MarkPending let tests drive the two halves of a round apart.
func (m *Manager) BeginRound(id, by string) error          { return m.beginRound(id, by) }
func (m *Manager) RunRound(ctx context.Context, id string) { m.runRound(ctx, id) }
