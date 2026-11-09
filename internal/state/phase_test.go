package state_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

// The order is the lifecycle in docs/formats.md §6.
var lifecycle = []domain.Phase{
	domain.PhasePlan, domain.PhaseImplement, domain.PhaseTest,
	domain.PhaseReview, domain.PhasePush, domain.PhaseDone,
}

func TestEveryPairOfPhases(t *testing.T) {
	for i, from := range lifecycle {
		for j, to := range lifecycle {
			wantOK := j == i+1 || (j < i && from != domain.PhaseDone)
			err := state.CanTransition(from, to)
			if (err == nil) != wantOK {
				t.Errorf("%s -> %s: error = %v, allowed should be %v", from, to, err, wantOK)
			}
		}
	}
}

func TestTransitionRulesExplainThemselves(t *testing.T) {
	tests := []struct {
		name     string
		from, to domain.Phase
		want     string // substring of the error; empty means allowed
	}{
		{"one step forward", domain.PhasePlan, domain.PhaseImplement, ""},
		{"the last step", domain.PhasePush, domain.PhaseDone, ""},
		{"back one step", domain.PhaseReview, domain.PhaseTest, ""},
		{"back to the start", domain.PhasePush, domain.PhasePlan, ""},
		{"skipping forward", domain.PhasePlan, domain.PhaseTest, "one step"},
		{"skipping to done", domain.PhaseReview, domain.PhaseDone, "one step"},
		{"staying put", domain.PhaseTest, domain.PhaseTest, "already"},
		{"leaving done", domain.PhaseDone, domain.PhasePlan, "terminal"},
		{"done to done", domain.PhaseDone, domain.PhaseDone, "terminal"},
		{"an unknown source phase", "daydream", domain.PhasePlan, "daydream"},
		{"an unknown target phase", domain.PhasePlan, "daydream", "daydream"},
		{"an empty phase", "", domain.PhasePlan, "phase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := state.CanTransition(tt.from, tt.to)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("error = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

func TestIsBackward(t *testing.T) {
	tests := []struct {
		from, to domain.Phase
		want     bool
	}{
		{domain.PhaseReview, domain.PhaseImplement, true},
		{domain.PhasePush, domain.PhasePlan, true},
		{domain.PhasePlan, domain.PhaseImplement, false},
		{domain.PhaseTest, domain.PhaseTest, false},
	}
	for _, tt := range tests {
		if got := state.IsBackward(tt.from, tt.to); got != tt.want {
			t.Errorf("IsBackward(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestNextPhase(t *testing.T) {
	for i, p := range lifecycle[:len(lifecycle)-1] {
		got, ok := state.NextPhase(p)
		if !ok || got != lifecycle[i+1] {
			t.Errorf("NextPhase(%s) = %s, %v, want %s", p, got, ok, lifecycle[i+1])
		}
	}
	if got, ok := state.NextPhase(domain.PhaseDone); ok {
		t.Errorf("NextPhase(done) = %s; done has no next phase", got)
	}
}

func TestAPhaseSurvivesAReload(t *testing.T) {
	m, _ := newManager()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Create(path, state.State{TicketID: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []domain.Phase{domain.PhaseImplement, domain.PhaseTest, domain.PhaseImplement} {
		if err := m.Update(path, func(s *state.State) error {
			if err := state.CanTransition(s.Phase, to); err != nil {
				return err
			}
			s.Phase = to
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got, err := m.Load(path); err != nil || got.Phase != to {
			t.Fatalf("after moving to %s: phase = %s, %v", to, got.Phase, err)
		}
	}
}
