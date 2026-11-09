//go:build integration

package herdr_test

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/terminal/herdr"
	"github.com/charlesrobsampson/rig/internal/terminal/terminaltest"
)

func TestTheBackendMeetsTheContractAgainstRealHerdr(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr is not installed")
	}
	terminaltest.Run(t, func(t *testing.T) terminaltest.Subject {
		session := fmt.Sprintf("rig-itest-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
		b := herdr.New(herdr.Options{Session: session})
		if err := b.Ready(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			exec.Command("herdr", "--session", session, "server", "stop").Run()
			exec.Command("herdr", "session", "delete", session).Run()
		})
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
