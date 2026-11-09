package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// agreeContract writes something under "## Agreed", which a multi-service ticket needs
// before its plan can be approved.
func agreeContract(t *testing.T, home, id string) {
	t.Helper()
	md := "# Contract: " + id + "\n\n## Agreed\nGET /x returns 200.\n\n## Proposed changes\n\n## Changelog\n"
	if err := os.WriteFile(filepath.Join(home, "tickets", id, "contract.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGatesShowsWhatIsRequiredAndApproveGrantsIt(t *testing.T) {
	home, d, repo := newEnv(t)
	other := testkit.NewRepo(t)
	newTicket(t, d, "demo", repo, other)

	stdout, stderr, code := rig(t, d, "gates", "demo")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"GATE", "plan_approval", "review_approval", "svc1", "svc2", "plan -> implement", "pending", "<- needed to leave plan"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("gates output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "manual_test_signoff") {
		t.Errorf("a gate no service requires is listed:\n%s", stdout)
	}

	if _, stderr, code := rig(t, d, "approve", "demo", "plan_approval"); code != 3 || !strings.Contains(stderr, "contract") {
		t.Errorf("approving with an empty contract: code %d, stderr %q", code, stderr)
	}
	agreeContract(t, home, "demo")
	stdout, stderr, code = rig(t, d, "approve", "demo", "plan_approval", "--service", "svc1")
	if code != 0 || !strings.Contains(stdout, "plan_approval approved for: svc1") || !strings.Contains(stdout, "still needed from: svc2") {
		t.Fatalf("approve: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	stdout, _, code = rig(t, d, "approve", "demo", "plan_approval")
	if code != 0 || !strings.Contains(stdout, "every service has approved plan_approval") {
		t.Errorf("approving the rest: code %d, stdout %q", code, stdout)
	}

	stdout, _, _ = rig(t, d, "gates", "demo")
	if strings.Count(stdout, "approved by human") != 2 {
		t.Errorf("both plan approvals should show as given:\n%s", stdout)
	}
}

func TestGatesJSON(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "approve", "demo", "plan_approval")
	stdout, _, code := rig(t, d, "gates", "demo", "--json")
	var got api.GateListResult
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || got.Phase != domain.PhasePlan || len(got.Gates) != 2 || !got.Gates[0].Approved || got.Gates[0].At == nil {
		t.Errorf("code %d, %q, %v", code, stdout, err)
	}
}

func TestApproveErrors(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	tests := []struct {
		name string
		args []string
		code int
		text string
	}{
		{"an unknown gate name", []string{"approve", "demo", "ship_it"}, 2, "ship_it"},
		{"missing arguments", []string{"approve", "demo"}, 2, "arg"},
		{"a gate for a later phase", []string{"approve", "demo", "review_approval"}, 1, "plan"},
		{"a gate nobody requires", []string{"approve", "demo", "manual_test_signoff"}, 2, "not required"},
		{"a service that does not exist", []string{"approve", "demo", "plan_approval", "--service", "ghost"}, 2, "ghost"},
		{"an unknown ticket", []string{"approve", "nope", "plan_approval"}, 5, "nope"},
		{"gates for an unknown ticket", []string{"gates", "nope"}, 5, "nope"},
		{"gates without a ticket", []string{"gates"}, 2, "arg"},
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
