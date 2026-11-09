//go:build integration

package tmux_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/terminal/terminaltest"
	"github.com/charlesrobsampson/rig/internal/terminal/tmux"
)

func TestTheBackendMeetsTheContractAgainstRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	ctx := context.Background()
	terminaltest.Run(t, func(t *testing.T) terminaltest.Subject {
		socket := fmt.Sprintf("rig-itest-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
		b := tmux.New(tmux.Options{Socket: socket})
		t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
		return terminaltest.Subject{
			Backend:       b,
			RunsProcesses: true,
			SpawnExiting: func(t *testing.T, sp terminal.SpawnSpec, code int) terminal.PaneID {
				sp.Command = []string{"sh", "-c", fmt.Sprintf("exit %d", code)}
				id, err := b.Spawn(ctx, sp)
				if err != nil {
					t.Fatal(err)
				}
				return id
			},
		}
	})
}

func realTmux(t *testing.T) *tmux.Backend {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("rig-itest-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	return tmux.New(tmux.Options{Socket: socket})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAFinishedPaneIsKeptWithItsExitStatus(t *testing.T) {
	b := realTmux(t)
	ctx := context.Background()
	id, err := b.Spawn(ctx, terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: realDir(t), Command: []string{"sh", "-c", "exit 7"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pane to be seen as exited", func() bool {
		p, err := b.Get(ctx, id)
		return err == nil && !p.Alive
	})
	p, _ := b.Get(ctx, id)
	if p.ExitStatus == nil || *p.ExitStatus != 7 {
		t.Errorf("exit status = %v, want 7", p.ExitStatus)
	}
	if err := b.Close(ctx, id); err != nil {
		t.Errorf("closing the kept pane: %v", err)
	}
}

func TestKeyNamesTypedAsTextStayText(t *testing.T) {
	b := realTmux(t)
	ctx := context.Background()
	id, err := b.Spawn(ctx, terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: realDir(t), Command: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Enter", "C-c", "-n", "--"} {
		if err := b.SendText(ctx, id, text); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the text on screen", func() bool {
		out, _ := b.Read(ctx, id, 0)
		return strings.Contains(out, "EnterC-c-n--")
	})
	if p, err := b.Get(ctx, id); err != nil || !p.Alive {
		t.Errorf("the pane should still be alive (C-c typed as text must not interrupt): %+v, %v", p, err)
	}
}

func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheScreenHeuristicSeesARealPane(t *testing.T) {
	b := realTmux(t)
	ctx := context.Background()
	id, err := b.Spawn(ctx, terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: realDir(t),
		Command: []string{"sh", "-c", `echo "✻ Pondering… (3s · esc to interrupt)"; cat`}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pane to look like a working agent", func() bool {
		p, err := b.Get(ctx, id)
		return err == nil && p.Heuristic == domain.AgentWorking
	})
}
