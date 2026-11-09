package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

const twoProposals = `# Contract: demo

## Agreed
Base.

## Proposed changes
### P-svc1-1 · from svc1 · open
**What:** add a field
**Why:** needed

### P-svc2-1 · from svc2 · open
**What:** drop a field
**Why:** unused

## Changelog
`

// contractEnv starts a two-service ticket whose agents are ready and whose contract has
// two open proposals.
func contractEnv(t *testing.T) (deps, *testkit.FakeTerminal, map[string]string) {
	t.Helper()
	home, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo, testkit.NewRepo(t))
	if err := os.WriteFile(filepath.Join(home, "tickets", "demo", "contract.md"), []byte(twoProposals), 0o600); err != nil {
		t.Fatal(err)
	}
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	panes := map[string]string{}
	for _, svc := range []string{"svc1", "svc2"} {
		t.Setenv("RIG_SERVICE", svc)
		rig(t, d, "agent-event", "idle")
	}
	list, _ := term.List(ctx)
	for _, p := range list {
		panes[p.Service] = string(p.ID)
	}
	if len(panes) != 2 {
		t.Fatalf("panes = %v", list)
	}
	return d, term, panes
}

// typed waits for the asynchronous notice to reach a pane.
func typed(term *testkit.FakeTerminal, pane string) string {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if in := term.Input(terminal.PaneID(pane)); len(in) > 0 {
			return in[0].Text
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

func TestContractListShowsOpenProposalsAndAllWithTheFlag(t *testing.T) {
	d, _, _ := contractEnv(t)
	stdout, stderr, code := rig(t, d, "contract", "list", "demo")
	if code != 0 || !strings.Contains(stdout, "P-svc1-1  from svc1  [open]") || !strings.Contains(stdout, "drop a field") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	rig(t, d, "contract", "accept", "demo", "P-svc1-1")
	stdout, _, _ = rig(t, d, "contract", "list", "demo")
	if strings.Contains(stdout, "P-svc1-1") || !strings.Contains(stdout, "P-svc2-1") {
		t.Errorf("open only: %q", stdout)
	}
	stdout, _, _ = rig(t, d, "contract", "list", "demo", "--all")
	if !strings.Contains(stdout, "P-svc1-1  from svc1  [accepted]") {
		t.Errorf("--all: %q", stdout)
	}
	if out, _, code := rig(t, d, "--json", "contract", "list", "demo"); code != 0 || !strings.Contains(out, `"proposals"`) {
		t.Errorf("json: %d %q", code, out)
	}
}

func TestAcceptingTellsEveryAgent(t *testing.T) {
	d, term, panes := contractEnv(t)
	stdout, stderr, code := rig(t, d, "contract", "accept", "demo", "P-svc1-1")
	if code != 0 || !strings.Contains(stdout, "P-svc1-1 accepted") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for svc, pane := range panes {
		if text := typed(term, pane); !strings.Contains(text, "P-svc1-1") || !strings.Contains(text, "accepted") {
			t.Errorf("%s was told %q", svc, text)
		}
	}
}

func TestRejectingTellsOnlyTheProposerWhy(t *testing.T) {
	d, term, panes := contractEnv(t)
	if _, stderr, code := rig(t, d, "contract", "reject", "demo", "P-svc2-1"); code != 2 {
		t.Errorf("without a reason: code %d, %q", code, stderr)
	}
	stdout, stderr, code := rig(t, d, "contract", "reject", "demo", "P-svc2-1", "we", "still", "need", "it")
	if code != 0 || !strings.Contains(stdout, "P-svc2-1 rejected") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if text := typed(term, panes["svc2"]); !strings.Contains(text, "rejected: we still need it") {
		t.Errorf("proposer was told %q", text)
	}
	time.Sleep(100 * time.Millisecond)
	if in := term.Input(terminal.PaneID(panes["svc1"])); len(in) != 0 {
		t.Errorf("the other service was told too: %v", in)
	}
}

func TestContractErrorsMapToExitCodes(t *testing.T) {
	d, _, _ := contractEnv(t)
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"an unknown proposal", []string{"contract", "accept", "demo", "P-svc1-9"}, 5},
		{"an unknown ticket", []string{"contract", "list", "ghost"}, 5},
		{"nothing to acknowledge", []string{"contract", "ack", "demo"}, 1},
		{"too few arguments", []string{"contract", "accept", "demo"}, 2},
	}
	for _, tt := range tests {
		if _, stderr, code := rig(t, d, tt.args...); code != tt.code {
			t.Errorf("%s: code %d, want %d (%s)", tt.name, code, tt.code, stderr)
		}
	}
	rig(t, d, "contract", "accept", "demo", "P-svc1-1")
	if _, _, code := rig(t, d, "contract", "accept", "demo", "P-svc1-1"); code != 1 {
		t.Errorf("a second accept: code %d, want 1 (conflict)", code)
	}
}
