package testkit_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/terminal/terminaltest"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestFakeTerminalSatisfiesTheBackendContract(t *testing.T) {
	terminaltest.Run(t, func(t *testing.T) terminaltest.Subject {
		fake := testkit.NewFakeTerminal()
		return terminaltest.Subject{
			Backend: fake,
			SpawnExiting: func(t *testing.T, spec terminal.SpawnSpec, status int) terminal.PaneID {
				id, err := fake.Spawn(context.Background(), spec)
				if err != nil {
					t.Fatal(err)
				}
				fake.Exit(id, status)
				return id
			},
		}
	})
}

func spawn(t *testing.T, f *testkit.FakeTerminal, service string) terminal.PaneID {
	t.Helper()
	id, err := f.Spawn(context.Background(), terminal.SpawnSpec{Ticket: "demo", Service: service, Dir: testkit.RealTempDir(t), Command: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFakeTerminalRecordsInputInOrder(t *testing.T) {
	f := testkit.NewFakeTerminal()
	id := spawn(t, f, "api")
	f.SendText(context.Background(), id, "[rig] contract changed")
	f.Submit(context.Background(), id)
	f.SendText(context.Background(), id, "second\nline")
	want := []testkit.Input{{Text: "[rig] contract changed"}, {Submit: true}, {Text: "second\nline"}}
	if got := f.Input(id); !reflect.DeepEqual(got, want) {
		t.Errorf("Input = %+v, want %+v", got, want)
	}
}

func TestFakeTerminalScriptedBehavior(t *testing.T) {
	ctx := context.Background()
	f := testkit.NewFakeTerminal()
	id := spawn(t, f, "api")

	f.SetHeuristic(id, domain.AgentBlocked)
	if p, _ := f.Get(ctx, id); p.Heuristic != domain.AgentBlocked {
		t.Errorf("heuristic = %q", p.Heuristic)
	}

	f.Print(id, "line1\nline2\nline3\n")
	if out, _ := f.Read(ctx, id, 2); out != "line2\nline3" {
		t.Errorf("Read(2) = %q, want the last two lines", out)
	}
	if out, _ := f.Read(ctx, id, 0); !strings.Contains(out, "line1") {
		t.Errorf("Read(0) = %q, want the whole screen", out)
	}

	f.Exit(id, 2)
	p, err := f.Get(ctx, id)
	if err != nil || p.Alive || p.ExitStatus == nil || *p.ExitStatus != 2 {
		t.Errorf("after Exit: %+v, %v", p, err)
	}
	if err := f.SendText(ctx, id, "x"); !errors.Is(err, terminal.ErrNotFound) {
		t.Errorf("typing into an exited pane = %v", err)
	}
	if out, err := f.Read(ctx, id, 0); err != nil || !strings.Contains(out, "line1") {
		t.Errorf("an exited pane's screen should stay readable (like tmux): %q, %v", out, err)
	}

	f.Vanish(id)
	if _, err := f.Get(ctx, id); !errors.Is(err, terminal.ErrNotFound) {
		t.Errorf("after Vanish: %v", err)
	}
}

func TestFakeTerminalCanGoDown(t *testing.T) {
	ctx := context.Background()
	f := testkit.NewFakeTerminal()
	id := spawn(t, f, "api")
	f.Down(errors.New("server crashed"))
	for name, err := range map[string]error{
		"Ready":    f.Ready(ctx),
		"SendText": f.SendText(ctx, id, "x"),
	} {
		if !errors.Is(err, terminal.ErrUnavailable) || !strings.Contains(err.Error(), "server crashed") {
			t.Errorf("%s while down = %v", name, err)
		}
	}
	if _, err := f.List(ctx); !errors.Is(err, terminal.ErrUnavailable) {
		t.Errorf("List while down = %v", err)
	}
	f.Up()
	if err := f.Ready(ctx); err != nil {
		t.Errorf("Ready after Up = %v", err)
	}
	if _, err := f.Get(ctx, id); err != nil {
		t.Errorf("the pane should survive an outage: %v", err)
	}
}

func TestFakeTerminalRecordsWhatWasSpawned(t *testing.T) {
	f := testkit.NewFakeTerminal()
	spawn(t, f, "api")
	spawn(t, f, "web")
	got := f.Spawned()
	if len(got) != 2 || got[0].Service != "api" || got[1].Service != "web" || got[0].Command[0] != "claude" {
		t.Errorf("Spawned = %+v", got)
	}
}
