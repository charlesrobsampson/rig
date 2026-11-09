package main

import (
	"context"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/tui"
)

func TestBareRigOnAPipePrintsTheListInsteadOfTheTUI(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	opened := false
	d.isTerminal = func() bool { return false }
	d.runTUI = func(context.Context, tui.Options) error { opened = true; return nil }
	stdout, stderr, code := rig(t, d)
	if code != 0 || opened || !strings.Contains(stdout, "demo") || !strings.Contains(stdout, "PHASE") {
		t.Errorf("code %d, opened %v, stdout %q, stderr %q", code, opened, stdout, stderr)
	}
}

func TestBareRigWithJSONNeverOpensTheTUI(t *testing.T) {
	_, d, _ := newEnv(t)
	opened := false
	d.isTerminal = func() bool { return true }
	d.runTUI = func(context.Context, tui.Options) error { opened = true; return nil }
	stdout, _, code := rig(t, d, "--json")
	if code != 0 || opened || !strings.Contains(stdout, `"tickets"`) {
		t.Errorf("code %d, opened %v, stdout %q", code, opened, stdout)
	}
}

func TestBareRigOnATerminalOpensTheTUIOnTheDaemon(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	d.isTerminal = func() bool { return true }
	var got []string
	d.runTUI = func(ctx context.Context, o tui.Options) error {
		tickets, err := o.Source.Tickets(ctx)
		if err != nil {
			return err
		}
		for _, tk := range tickets {
			got = append(got, tk.ID)
		}
		return nil
	}
	if _, stderr, code := rig(t, d); code != 0 || strings.Join(got, ",") != "demo" {
		t.Errorf("code %d, tickets %v, stderr %q", code, got, stderr)
	}
}

func TestBareRigRejectsStrayArguments(t *testing.T) {
	_, d, _ := newEnv(t)
	if _, _, code := rig(t, d, "bogus"); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}
