package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func TestPhaseIsRefusedUntilApprovedThenMoves(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)

	stdout, stderr, code := rig(t, d, "phase", "demo", "implement")
	if code != 3 || stdout != "" {
		t.Fatalf("code %d (want 3), stdout %q", code, stdout)
	}
	for _, want := range []string{"plan_approval", "rig approve demo plan_approval", "  - plan_approval: svc1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "plan") || strings.Contains(listing, "implement") {
		t.Errorf("a refused move changed the phase:\n%s", listing)
	}

	if _, stderr, code := rig(t, d, "approve", "demo", "plan_approval"); code != 0 {
		t.Fatalf("approve failed: %s", stderr)
	}
	stdout, stderr, code = rig(t, d, "phase", "demo", "implement")
	if code != 0 || !strings.Contains(stdout, "demo: plan -> implement") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "implement") {
		t.Errorf("list after the move:\n%s", listing)
	}
}

func TestPhaseBackwardNeedsAReason(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "approve", "demo", "plan_approval")
	if _, stderr, code := rig(t, d, "phase", "demo", "implement"); code != 0 {
		t.Fatalf("setup failed: %s", stderr)
	}

	_, stderr, code := rig(t, d, "phase", "demo", "plan")
	if code != 2 || !strings.Contains(stderr, "reason") {
		t.Errorf("code %d (want 2), stderr %q", code, stderr)
	}
	stdout, stderr, code := rig(t, d, "phase", "demo", "plan", "--reason", "scope change")
	if code != 0 || !strings.Contains(stdout, "implement -> plan") {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// The plan approval no longer holds, so going forward again is blocked.
	if _, _, code := rig(t, d, "phase", "demo", "implement"); code != 3 {
		t.Errorf("code %d, want 3: the plan must be approved again", code)
	}
}

func TestPhaseJSONAndErrors(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "approve", "demo", "plan_approval")
	stdout, _, code := rig(t, d, "phase", "demo", "implement", "--json")
	var got api.TransitionResult
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || got.From != domain.PhasePlan || got.To != domain.PhaseImplement || got.By != "human" {
		t.Errorf("code %d, %q, %v", code, stdout, err)
	}

	tests := []struct {
		name string
		args []string
		code int
		text string
	}{
		{"skipping a phase", []string{"phase", "demo", "review"}, 1, "one step"},
		{"staying put", []string{"phase", "demo", "implement"}, 1, "already"},
		{"an unknown phase name", []string{"phase", "demo", "daydream"}, 2, "daydream"},
		{"missing arguments", []string{"phase", "demo"}, 2, "arg"},
		{"an unknown ticket", []string{"phase", "nope", "implement"}, 5, "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := rig(t, d, tt.args...)
			if code != tt.code || !strings.Contains(stderr, tt.text) || stdout != "" {
				t.Errorf("code %d (want %d), stdout %q, stderr %q", code, tt.code, stdout, stderr)
			}
		})
	}
}
