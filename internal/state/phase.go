package state

import (
	"fmt"
	"slices"

	"github.com/charlesrobsampson/rig/internal/domain"
)

// lifecycle is the order a ticket moves through (docs/formats.md §6).
var lifecycle = []domain.Phase{
	domain.PhasePlan, domain.PhaseImplement, domain.PhaseTest,
	domain.PhaseReview, domain.PhasePush, domain.PhaseDone,
}

// CanTransition says whether a ticket may move from one phase to another, and if not,
// why. Forward moves go one step at a time; backward moves may go to any earlier
// phase; done is terminal. It checks only the order, not gates or reasons.
func CanTransition(from, to domain.Phase) error {
	i, j := slices.Index(lifecycle, from), slices.Index(lifecycle, to)
	switch {
	case i < 0:
		return fmt.Errorf("invalid phase %q", from)
	case j < 0:
		return fmt.Errorf("invalid phase %q", to)
	case from == domain.PhaseDone:
		return fmt.Errorf("a ticket in phase done is terminal and cannot move to %s", to)
	case i == j:
		return fmt.Errorf("the ticket is already in phase %s", from)
	case j > i+1:
		return fmt.Errorf("a ticket moves forward one step at a time: %s goes to %s, not %s", from, lifecycle[i+1], to)
	}
	return nil
}

// IsBackward reports whether the move goes to an earlier phase.
func IsBackward(from, to domain.Phase) bool {
	i, j := slices.Index(lifecycle, from), slices.Index(lifecycle, to)
	return i >= 0 && j >= 0 && j < i
}

// NextPhase is the phase after p, or false for done and unknown phases.
func NextPhase(p domain.Phase) (domain.Phase, bool) {
	i := slices.Index(lifecycle, p)
	if i < 0 || i == len(lifecycle)-1 {
		return "", false
	}
	return lifecycle[i+1], true
}
