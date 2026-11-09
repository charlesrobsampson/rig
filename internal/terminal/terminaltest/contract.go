// Package terminaltest is the behavior every terminal.Backend must have. Each
// implementation, including the in-memory fake, runs the same suite.
package terminaltest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/terminal"
)

// Subject is a backend under test and the few things the suite cannot do through the
// Backend interface alone.
type Subject struct {
	Backend terminal.Backend
	// RunsProcesses is true when panes really execute their command (real backends), so
	// the suite can check the working directory, environment, and argument handling.
	RunsProcesses bool
	// SpawnExiting starts a pane whose process ends with the given exit status. It
	// hides how each implementation makes a process end.
	SpawnExiting func(t *testing.T, spec terminal.SpawnSpec, status int) terminal.PaneID
}

// Run executes the suite, asking newSubject for a fresh backend for every case.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	ctx := context.Background()
	spec := func(t *testing.T, ticket, service string) terminal.SpawnSpec {
		return terminal.SpawnSpec{Ticket: ticket, Service: service, Dir: realDir(t), Command: []string{"cat"}}
	}

	t.Run("Ready can be called repeatedly", func(t *testing.T) {
		s := newSubject(t)
		for i := 0; i < 3; i++ {
			if err := s.Backend.Ready(ctx); err != nil {
				t.Fatalf("Ready #%d: %v", i+1, err)
			}
		}
	})

	t.Run("a spawned pane is listed and can be fetched", func(t *testing.T) {
		s := newSubject(t)
		id := mustSpawn(t, s, spec(t, "demo", "api"))
		if id == "" {
			t.Fatal("Spawn returned an empty pane id")
		}
		panes := mustList(t, s)
		if len(panes) != 1 || panes[0].ID != id || panes[0].Ticket != "demo" || panes[0].Service != "api" || !panes[0].Alive {
			t.Fatalf("List = %+v, want one live pane demo/api", panes)
		}
		got, err := s.Backend.Get(ctx, id)
		if err != nil || got.ID != id || got.Ticket != "demo" || got.Service != "api" || !got.Alive {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if !got.Heuristic.Valid() {
			t.Errorf("Heuristic = %q, want a valid agent state (unknown when there is no guess)", got.Heuristic)
		}
	})

	t.Run("several services of one ticket get separate panes", func(t *testing.T) {
		s := newSubject(t)
		seen := map[terminal.PaneID]bool{}
		for _, svc := range []string{"api", "web", "worker", "db", "cache"} {
			seen[mustSpawn(t, s, spec(t, "demo", svc))] = true
		}
		if len(seen) != 5 || len(mustList(t, s)) != 5 {
			t.Errorf("ids = %d distinct, listed = %d, want 5 of each", len(seen), len(mustList(t, s)))
		}
	})

	t.Run("a bad spawn is refused and leaves nothing behind", func(t *testing.T) {
		s := newSubject(t)
		good := spec(t, "demo", "api")
		bad := map[string]func(terminal.SpawnSpec) terminal.SpawnSpec{
			"a missing directory":  func(sp terminal.SpawnSpec) terminal.SpawnSpec { sp.Dir = filepath.Join(sp.Dir, "nope"); return sp },
			"a relative directory": func(sp terminal.SpawnSpec) terminal.SpawnSpec { sp.Dir = "."; return sp },
			"no ticket":            func(sp terminal.SpawnSpec) terminal.SpawnSpec { sp.Ticket = ""; return sp },
			"no service":           func(sp terminal.SpawnSpec) terminal.SpawnSpec { sp.Service = ""; return sp },
		}
		for name, mutate := range bad {
			if _, err := s.Backend.Spawn(ctx, mutate(good)); !errors.Is(err, terminal.ErrInvalid) {
				t.Errorf("%s: error = %v, want ErrInvalid", name, err)
			}
		}
		if panes := mustList(t, s); len(panes) != 0 {
			t.Errorf("failed spawns left panes behind: %+v", panes)
		}
	})

	t.Run("a ticket cannot have two panes for one service", func(t *testing.T) {
		s := newSubject(t)
		mustSpawn(t, s, spec(t, "demo", "api"))
		if _, err := s.Backend.Spawn(ctx, spec(t, "demo", "api")); !errors.Is(err, terminal.ErrExists) {
			t.Errorf("error = %v, want ErrExists", err)
		}
		// Another ticket may use the same service name.
		mustSpawn(t, s, spec(t, "other", "api"))
		if n := len(mustList(t, s)); n != 2 {
			t.Errorf("panes = %d, want 2", n)
		}
	})

	t.Run("a cancelled context stops a spawn", func(t *testing.T) {
		s := newSubject(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Backend.Spawn(cancelled, spec(t, "demo", "api")); err == nil {
			t.Error("expected an error")
		}
		if n := len(mustList(t, s)); n != 0 {
			t.Errorf("a cancelled spawn left %d pane(s)", n)
		}
	})

	t.Run("an unknown pane is reported as not found by every operation", func(t *testing.T) {
		s := newSubject(t)
		ghost := terminal.PaneID("no-such-pane")
		checks := map[string]error{}
		_, checks["Get"] = s.Backend.Get(ctx, ghost)
		_, checks["Read"] = s.Backend.Read(ctx, ghost, 0)
		checks["SendText"] = s.Backend.SendText(ctx, ghost, "x")
		checks["Submit"] = s.Backend.Submit(ctx, ghost)
		checks["Close"] = s.Backend.Close(ctx, ghost)
		_, checks["Attach"] = s.Backend.Attach(ctx, ghost)
		for op, err := range checks {
			if !errors.Is(err, terminal.ErrNotFound) {
				t.Errorf("%s: error = %v, want ErrNotFound", op, err)
			}
		}
	})

	t.Run("typed text arrives in order and exactly", func(t *testing.T) {
		s := newSubject(t)
		id := mustSpawn(t, s, spec(t, "demo", "api"))
		for _, text := range []string{"first line", `quotes "x" and $HOME and ` + "`ticks`", "héllo ✓ wörld"} {
			if err := s.Backend.SendText(ctx, id, text); err != nil {
				t.Fatal(err)
			}
			if err := s.Backend.Submit(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		out := waitForOutput(t, s, id, "héllo ✓ wörld")
		i1, i2, i3 := strings.Index(out, "first line"), strings.Index(out, `quotes "x" and $HOME and `+"`ticks`"), strings.Index(out, "héllo ✓ wörld")
		if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
			t.Errorf("output does not show the three lines in order (%d, %d, %d):\n%s", i1, i2, i3, out)
		}
	})

	t.Run("a newline inside text is part of the text", func(t *testing.T) {
		s := newSubject(t)
		id := mustSpawn(t, s, spec(t, "demo", "api"))
		if err := s.Backend.SendText(ctx, id, "line one\nline two"); err != nil {
			t.Fatal(err)
		}
		out := waitForOutput(t, s, id, "line two")
		if !strings.Contains(out, "line one") {
			t.Errorf("output lacks the first line:\n%s", out)
		}
	})

	t.Run("input goes only to the pane it was sent to", func(t *testing.T) {
		s := newSubject(t)
		a := mustSpawn(t, s, spec(t, "demo", "api"))
		b := mustSpawn(t, s, spec(t, "demo", "web"))
		s.Backend.SendText(ctx, a, "for-api-only")
		s.Backend.Submit(ctx, a)
		waitForOutput(t, s, a, "for-api-only")
		if out, _ := s.Backend.Read(ctx, b, 0); strings.Contains(out, "for-api-only") {
			t.Errorf("web's pane received api's input:\n%s", out)
		}
	})

	t.Run("a closed pane is gone", func(t *testing.T) {
		s := newSubject(t)
		id := mustSpawn(t, s, spec(t, "demo", "api"))
		keep := mustSpawn(t, s, spec(t, "demo", "web"))
		if err := s.Backend.Close(ctx, id); err != nil {
			t.Fatal(err)
		}
		panes := mustList(t, s)
		if len(panes) != 1 || panes[0].ID != keep {
			t.Errorf("List after Close = %+v, want only the other pane", panes)
		}
		if _, err := s.Backend.Get(ctx, id); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("Get = %v, want ErrNotFound", err)
		}
		if err := s.Backend.SendText(ctx, id, "x"); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("SendText = %v, want ErrNotFound", err)
		}
		if err := s.Backend.Close(ctx, id); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("a second Close = %v, want ErrNotFound", err)
		}
		// The service can be spawned again once its pane is closed.
		mustSpawn(t, s, spec(t, "demo", "api"))
	})

	t.Run("a live pane can be attached to", func(t *testing.T) {
		s := newSubject(t)
		id := mustSpawn(t, s, spec(t, "demo", "api"))
		cmd, err := s.Backend.Attach(ctx, id)
		if err != nil || len(cmd) == 0 || cmd[0] == "" {
			t.Errorf("Attach = %v, %v, want a command", cmd, err)
		}
	})

	t.Run("a process that exits is seen as exited", func(t *testing.T) {
		s := newSubject(t)
		id := s.SpawnExiting(t, spec(t, "demo", "api"), 3)
		eventually(t, 10*time.Second, "the pane to be reported as exited", func() bool {
			p, err := s.Backend.Get(ctx, id)
			if errors.Is(err, terminal.ErrNotFound) {
				return true // some backends drop a pane whose process ended
			}
			if err != nil || p.Alive {
				return false
			}
			if p.ExitStatus != nil && *p.ExitStatus != 3 {
				t.Errorf("exit status = %d, want 3", *p.ExitStatus)
			}
			return true
		})
		if err := s.Backend.SendText(ctx, id, "too late"); !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("typing into an exited pane = %v, want ErrNotFound", err)
		}
		if err := s.Backend.Close(ctx, id); err != nil && !errors.Is(err, terminal.ErrNotFound) {
			t.Errorf("closing an exited pane = %v", err)
		}
	})

	t.Run("real processes", func(t *testing.T) {
		probe := newSubject(t)
		if !probe.RunsProcesses {
			t.Skip("this backend does not run processes")
		}

		t.Run("start in the requested directory with the requested environment", func(t *testing.T) {
			s := newSubject(t)
			dir := realDir(t)
			sp := terminal.SpawnSpec{
				Ticket: "demo", Service: "api", Dir: dir,
				Command: []string{"sh", "-c", `echo "cwd=$(pwd)"; echo "env=$RIG_TEST_VALUE"; cat`},
				Env:     map[string]string{"RIG_TEST_VALUE": "hello-env"},
			}
			id := mustSpawn(t, s, sp)
			out := waitForOutput(t, s, id, "env=hello-env")
			if !strings.Contains(out, "cwd="+dir) {
				t.Errorf("the pane did not start in %s:\n%s", dir, out)
			}
		})

		t.Run("arguments are passed exactly, spaces and quotes included", func(t *testing.T) {
			s := newSubject(t)
			sp := terminal.SpawnSpec{
				Ticket: "demo", Service: "api", Dir: realDir(t),
				Command: []string{"sh", "-c", `printf '[%s]' "$@"; echo; cat`, "sh", "a b", `c"d`, "e$f"},
			}
			id := mustSpawn(t, s, sp)
			waitForOutput(t, s, id, `[a b][c"d][e$f]`)
		})

		t.Run("the exit status is reported when the backend keeps the pane", func(t *testing.T) {
			s := newSubject(t)
			id := s.SpawnExiting(t, spec(t, "demo", "api"), 7)
			eventually(t, 10*time.Second, "an exit status or a dropped pane", func() bool {
				p, err := s.Backend.Get(ctx, id)
				if errors.Is(err, terminal.ErrNotFound) {
					return true
				}
				return err == nil && !p.Alive && p.ExitStatus != nil && *p.ExitStatus == 7
			})
		})
	})
}

func mustSpawn(t *testing.T, s Subject, spec terminal.SpawnSpec) terminal.PaneID {
	t.Helper()
	id, err := s.Backend.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("Spawn %s/%s: %v", spec.Ticket, spec.Service, err)
	}
	return id
}

func mustList(t *testing.T, s Subject) []terminal.Pane {
	t.Helper()
	panes, err := s.Backend.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return panes
}

func waitForOutput(t *testing.T, s Subject, id terminal.PaneID, want string) string {
	t.Helper()
	var out string
	eventually(t, 10*time.Second, "the pane to show "+want, func() bool {
		var err error
		out, err = s.Backend.Read(context.Background(), id, 0)
		return err == nil && strings.Contains(out, want)
	})
	return out
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// realDir is an existing absolute directory with symlinks resolved (a pane reports its
// real working directory).
func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
