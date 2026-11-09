//go:build integration

package inject_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/inject"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/terminal/tmux"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// The fake agent's wait-input records what reaches its terminal, so this shows the
// exact bytes a real pane receives through the whole injection path.
func TestTheFakeAgentReceivesTheExactTextThroughRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	work := testkit.RealTempDir(t)
	bin := filepath.Join(work, "fakeagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/charlesrobsampson/rig/tools/fakeagent").CombinedOutput(); err != nil {
		t.Fatalf("build fakeagent: %v\n%s", err, out)
	}
	received := filepath.Join(work, "received.txt")
	script := filepath.Join(work, "script.txt")
	os.WriteFile(script, []byte("wait-input "+received+"\n"), 0o600)

	socket := fmt.Sprintf("rig-itest-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	backend := tmux.New(tmux.Options{Socket: socket})
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	pane, err := backend.Spawn(ctx, terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: work, Command: []string{bin, "-script", script}})
	if err != nil {
		t.Fatal(err)
	}

	tickets := filepath.Join(work, "tickets")
	os.MkdirAll(filepath.Join(tickets, "demo"), 0o700)
	states := state.NewManager(testkit.NewFakeClock(epoch))
	err = states.Create(filepath.Join(tickets, "demo", "state.json"), state.State{TicketID: "demo", Services: map[string]state.ServiceState{
		"api": {Agent: state.Agent{Backend: "tmux", PaneID: string(pane), State: domain.AgentIdle}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	in := inject.New(inject.Options{Dir: tickets, States: states, Backend: backend, Readiness: &readyFlag{on: true}, Pause: 100 * time.Millisecond})

	// Single-line text travels as send-keys, multi-line as a paste; both must arrive whole.
	text := "line one: it's \"quoted\" $HOME `id` $(id) ✓\nline two\twith a tab"
	if _, err := in.Send(ctx, api.AgentMessageParams{Ticket: "demo", Service: "api", Text: text}, "human"); err != nil {
		t.Fatal(err)
	}
	want := "[rig] " + text
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, _ := os.ReadFile(received)
		if strings.TrimRight(string(raw), "\n") == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent received %q, want %q", raw, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
