package examples_test

import (
	"path/filepath"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
)

// The examples are documentation people copy from, so they must keep parsing.
func TestTheExampleTicketsAreValid(t *testing.T) {
	files, _ := filepath.Glob("tickets/*.yaml")
	if len(files) < 3 {
		t.Fatalf("expected the example tickets, found %v", files)
	}
	for _, f := range files {
		if _, err := config.LoadTicket(f, config.TicketOptions{RepoCheck: func(string) error { return nil }}); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestTheExampleConfigIsValid(t *testing.T) {
	path, _ := filepath.Abs("config.json")
	home := t.TempDir()
	g, err := config.LoadGlobal(config.GlobalOptions{Path: path, Env: func(string) string { return "" }, UserHome: func() (string, error) { return home, nil }})
	if err != nil {
		t.Fatalf("config.json: %v", err)
	}
	if g.Limits.MaxActiveTickets != 4 || g.Defaults.TerminalBackend != "tmux" || g.Jira.BaseURL == "" {
		t.Errorf("the example config did not load as written: %+v", g)
	}
}
