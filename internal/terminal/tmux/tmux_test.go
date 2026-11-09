package tmux_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/terminal/tmux"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestHeuristicReadsClaudeCodesScreen(t *testing.T) {
	tests := []struct {
		name, screen string
		want         domain.AgentState
	}{
		{"working shows the interrupt hint", "● Reading files\n\n✻ Pondering… (12s · esc to interrupt)\n", domain.AgentWorking},
		{"a permission dialog is blocked", "Bash command\n  rm -rf build\n\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No\n", domain.AgentBlocked},
		{"blocked wins over a stale working line", "✻ Pondering… (esc to interrupt)\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n", domain.AgentBlocked},
		{"a question in prose is not a dialog", "I asked: Do you want to proceed with the refactor?\n\n╭──────╮\n│ >    │\n╰──────╯\n  ? for shortcuts\n", domain.AgentIdle},
		{"the input box is idle", "✻ Worked for 4s\n\n╭──────────╮\n│ >        │\n╰──────────╯\n  ? for shortcuts\n", domain.AgentIdle},
		{"a plain shell is unknown", "charley@host ~/code $ ls\nfile.go\n", domain.AgentUnknown},
		{"a blank screen is unknown", "\n\n\n", domain.AgentUnknown},
		{"an empty screen is unknown", "", domain.AgentUnknown},
		{"old output scrolled far up does not count", "Do you want to proceed?\n1. Yes\n" + strings.Repeat("output line\n", 40), domain.AgentUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tmux.Heuristic(tt.screen); got != tt.want {
				t.Errorf("heuristic = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestVersionCheck(t *testing.T) {
	tests := []struct {
		out  string
		want bool
	}{
		{"tmux 3.8\n", true},
		{"tmux 3.2\n", true},
		{"tmux 3.2a\n", true},
		{"tmux 3.1c\n", false},
		{"tmux 2.9\n", false},
		{"tmux 10.0\n", true},
		{"tmux next-3.4\n", true},
		{"tmux master\n", false},
		{"", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		if got := tmux.AtLeast(tt.out, 3, 2); got != tt.want {
			t.Errorf("AtLeast(%q) = %v, want %v", tt.out, got, tt.want)
		}
	}
}

// fakeTmux records its arguments (one line per call) and prints canned output for -V.
func fakeTmux(t *testing.T, version string) (binary, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	binary = filepath.Join(dir, "tmux")
	script := "#!/bin/sh\n" +
		`if [ "$1" = "-V" ]; then echo '` + version + `'; exit 0; fi` + "\n" +
		`echo "$*" >> ` + log + "\n" +
		`echo "no server running on /tmp/x" >&2; exit 1` + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, log
}

func TestReadyRequiresARecentEnoughTmux(t *testing.T) {
	ctx := context.Background()
	old, _ := fakeTmux(t, "tmux 3.0a")
	current, _ := fakeTmux(t, "tmux 3.5")
	tests := []struct {
		name, binary, want string
	}{
		{"recent", current, ""},
		{"too old", old, "3.2 or newer"},
		{"missing", "no-such-tmux-binary-xyz", "not installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tmux.New(tmux.Options{Binary: tt.binary}).Ready(ctx)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, terminal.ErrUnavailable) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want ErrUnavailable mentioning %q", err, tt.want)
			}
		})
	}
}

func TestBadRequestsAreRefusedBeforeTmuxIsTouched(t *testing.T) {
	binary, log := fakeTmux(t, "tmux 3.5")
	b := tmux.New(tmux.Options{Binary: binary})
	dir := testkit.RealTempDir(t)
	tests := []struct {
		name string
		spec terminal.SpawnSpec
	}{
		{"no ticket", terminal.SpawnSpec{Service: "api", Dir: dir}},
		{"no service", terminal.SpawnSpec{Ticket: "demo", Dir: dir}},
		{"a dot in the ticket (tmux rewrites it)", terminal.SpawnSpec{Ticket: "a.b", Service: "api", Dir: dir}},
		{"a colon in the ticket (tmux target syntax)", terminal.SpawnSpec{Ticket: "a:b", Service: "api", Dir: dir}},
		{"relative directory", terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: "."}},
		{"missing directory", terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: filepath.Join(dir, "nope")}},
		{"bad environment name", terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: dir, Env: map[string]string{"A=B": "x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := b.Spawn(context.Background(), tt.spec); !errors.Is(err, terminal.ErrInvalid) {
				t.Errorf("Spawn = %v, want ErrInvalid", err)
			}
		})
	}
	if raw, _ := os.ReadFile(log); len(raw) != 0 {
		t.Errorf("tmux was called for refused requests: %s", raw)
	}
}

func TestPaneIDsThatCouldBeOptionsAreNotFound(t *testing.T) {
	binary, log := fakeTmux(t, "tmux 3.5")
	b := tmux.New(tmux.Options{Binary: binary})
	ctx := context.Background()
	for _, id := range []terminal.PaneID{"-t", "", "%x", "%1;kill-server", "demo:api"} {
		if _, err := b.Get(ctx, id); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("Get(%q) = %v", id, err)
		}
		if err := b.SendText(ctx, id, "x"); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("SendText(%q) = %v", id, err)
		}
		if err := b.Submit(ctx, id); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("Submit(%q) = %v", id, err)
		}
		if err := b.Close(ctx, id); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("Close(%q) = %v", id, err)
		}
	}
	if raw, _ := os.ReadFile(log); len(raw) != 0 {
		t.Errorf("tmux was called with a malformed pane id: %s", raw)
	}
}

func TestNoServerMeansNoPanes(t *testing.T) {
	binary, _ := fakeTmux(t, "tmux 3.5")
	b := tmux.New(tmux.Options{Binary: binary})
	panes, err := b.List(context.Background())
	if err != nil || len(panes) != 0 {
		t.Errorf("List with no server = %v, %v; want empty and no error", panes, err)
	}
	if _, err := b.Get(context.Background(), "%1"); !errors.Is(err, terminal.ErrNotFound) {
		t.Errorf("Get with no server = %v, want ErrNotFound", err)
	}
}

func TestPrivateServerArgumentsAndAttachCommand(t *testing.T) {
	binary, log := fakeTmux(t, "tmux 3.5")
	b := tmux.New(tmux.Options{Binary: binary, Socket: "mysock"})
	b.List(context.Background())
	raw, _ := os.ReadFile(log)
	if !strings.HasPrefix(string(raw), "-f /dev/null -L mysock ") {
		t.Errorf("tmux was called as %q, want it to ignore user config and use the private socket", raw)
	}
}
