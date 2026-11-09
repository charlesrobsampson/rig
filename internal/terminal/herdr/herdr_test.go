package herdr_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/terminal/herdr"
	"github.com/charlesrobsampson/rig/internal/terminal/terminaltest"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var ctx = context.Background()

func backendFor(f *fakeHerdr) *herdr.Backend {
	return herdr.New(herdr.Options{Session: "rig", Socket: f.path})
}

func spec(t *testing.T, ticket, service string, command ...string) terminal.SpawnSpec {
	return terminal.SpawnSpec{Ticket: ticket, Service: service, Dir: testkit.RealTempDir(t), Command: command}
}

func TestTheBackendMeetsTheContractAgainstAFakeHerdr(t *testing.T) {
	terminaltest.Run(t, func(t *testing.T) terminaltest.Subject {
		fake := startFakeHerdr(t)
		b := backendFor(fake)
		return terminaltest.Subject{
			Backend: b,
			SpawnExiting: func(t *testing.T, sp terminal.SpawnSpec, _ int) terminal.PaneID {
				id, err := b.Spawn(ctx, sp)
				if err != nil {
					t.Fatal(err)
				}
				fake.exitPane(string(id))
				return id
			},
		}
	})
}

func TestShellLineQuotesEveryWordExactly(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"plain words", []string{"one", "two"}},
		{"spaces", []string{"a b", "c  d"}},
		{"single quotes", []string{"it's", "'"}},
		{"double quotes", []string{`say "hi"`}},
		{"dollar and backticks", []string{"$HOME", "`id`", "$(id)"}},
		{"backslashes", []string{`a\b`, `\`, `\n`}},
		{"a newline", []string{"line1\nline2"}},
		{"an empty argument", []string{"", "after"}},
		{"unicode", []string{"héllo ✓"}},
		{"a leading dash", []string{"-n", "--flag=1"}},
		{"shell metacharacters", []string{"a;b", "a&&b", "a|b", "*", "?", "~"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			words := append([]string{"sh", "-c", `printf '[%s]' "$@"`, "sh"}, tt.args...)
			line, err := herdr.ShellLine(words, nil)
			if err != nil {
				t.Fatal(err)
			}
			// What a shell does with the typed line, minus the exec of a real program.
			out, err := exec.Command("sh", "-c", line).Output()
			if err != nil {
				t.Fatalf("running %q: %v", line, err)
			}
			var want strings.Builder
			for _, a := range tt.args {
				want.WriteString("[" + a + "]")
			}
			if string(out) != want.String() {
				t.Errorf("line %q printed %q, want %q", line, out, want.String())
			}
		})
	}
}

func TestShellLineEnvironment(t *testing.T) {
	line, err := herdr.ShellLine([]string{"sh", "-c", `printf '%s|%s' "$RIG_A" "$RIG_B"`}, map[string]string{"RIG_B": "it's $HOME", "RIG_A": "a b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "exec env RIG_A=") {
		t.Errorf("line = %q, want keys in sorted order after exec env", line)
	}
	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil || string(out) != "a b|it's $HOME" {
		t.Errorf("env round trip = %q, %v", out, err)
	}
	for _, bad := range []string{"", "1ABC", "A-B", "A B", "A=B"} {
		if _, err := herdr.ShellLine([]string{"x"}, map[string]string{bad: "v"}); err == nil {
			t.Errorf("environment variable name %q was accepted", bad)
		}
	}
}

func TestSpawnBuildsTheWorkspaceAndTabsTheSpikeDescribes(t *testing.T) {
	fake := startFakeHerdr(t)
	b := backendFor(fake)

	api := mustSpawn(t, b, spec(t, "demo", "api", "claude", "--flag"))
	web := mustSpawn(t, b, spec(t, "demo", "web", "bun", "dev"))
	other := mustSpawn(t, b, spec(t, "other", "api", "cat"))

	got := fake.snapshot()
	want := map[string][]string{"demo": {"api", "web"}, "other": {"api"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("herdr state = %v, want one workspace per ticket and one tab per service: %v", got, want)
	}
	if n := fake.callCount("workspace.create"); n != 2 {
		t.Errorf("workspace.create called %d times, want 2 (the second service reuses its ticket's workspace)", n)
	}

	// The first service takes over the workspace's initial tab (renamed), later ones add tabs.
	if n := fake.callCount("tab.rename"); n != 2 {
		t.Errorf("tab.rename called %d times, want one per new workspace", n)
	}
	if n := fake.callCount("tab.create"); n != 1 {
		t.Errorf("tab.create called %d times, want 1 (web)", n)
	}

	if typed := fake.typed(string(api)); !reflect.DeepEqual(typed, []string{"exec 'claude' '--flag'", "<Enter>"}) {
		t.Errorf("typed into api = %q", typed)
	}
	if typed := fake.typed(string(web)); !reflect.DeepEqual(typed, []string{"exec 'bun' 'dev'", "<Enter>"}) {
		t.Errorf("typed into web = %q", typed)
	}

	panes, _ := b.List(ctx)
	byID := map[terminal.PaneID]terminal.Pane{}
	for _, p := range panes {
		byID[p.ID] = p
	}
	if p := byID[other]; p.Ticket != "other" || p.Service != "api" {
		t.Errorf("other = %+v", p)
	}
}

func mustSpawn(t *testing.T, b *herdr.Backend, sp terminal.SpawnSpec) terminal.PaneID {
	t.Helper()
	id, err := b.Spawn(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSpawnWithoutACommandLeavesTheShell(t *testing.T) {
	fake := startFakeHerdr(t)
	id := mustSpawn(t, backendFor(fake), spec(t, "demo", "api"))
	if typed := fake.typed(string(id)); len(typed) != 0 {
		t.Errorf("typed %q into a pane that had no command", typed)
	}
}

func TestSpawnTypesTheEnvironmentIntoTheCommand(t *testing.T) {
	fake := startFakeHerdr(t)
	sp := spec(t, "demo", "api", "claude")
	sp.Env = map[string]string{"RIG_TICKET": "demo", "RIG_SERVICE": "api"}
	id := mustSpawn(t, backendFor(fake), sp)
	if typed := fake.typed(string(id)); typed[0] != "exec env RIG_SERVICE='api' RIG_TICKET='demo' 'claude'" {
		t.Errorf("typed %q", typed)
	}
	sp = spec(t, "demo", "web", "x")
	sp.Env = map[string]string{"BAD-NAME": "v"}
	if _, err := backendFor(fake).Spawn(ctx, sp); !errors.Is(err, terminal.ErrInvalid) {
		t.Errorf("a bad environment name = %v, want ErrInvalid", err)
	}
}

func TestClosingTheLastPaneRemovesTheTicketAndTheNameIsFreeAgain(t *testing.T) {
	fake := startFakeHerdr(t)
	b := backendFor(fake)
	id := mustSpawn(t, b, spec(t, "demo", "api", "cat"))
	if err := b.Close(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := fake.snapshot(); len(got) != 0 {
		t.Errorf("herdr state = %v; an empty workspace should be gone", got)
	}
	mustSpawn(t, b, spec(t, "demo", "api", "cat"))
}

func TestAgentStatusBecomesTheHeuristic(t *testing.T) {
	fake := startFakeHerdr(t)
	b := backendFor(fake)
	id := mustSpawn(t, b, spec(t, "demo", "api", "claude"))
	tests := map[string]domain.AgentState{
		"working": domain.AgentWorking,
		"blocked": domain.AgentBlocked,
		"idle":    domain.AgentIdle,
		"done":    domain.AgentIdle, // finished but not yet seen
		"unknown": domain.AgentUnknown,
		"mystery": domain.AgentUnknown,
	}
	for status, want := range tests {
		fake.setStatus(string(id), status)
		p, err := b.Get(ctx, id)
		if err != nil || p.Heuristic != want || !p.Alive {
			t.Errorf("status %q: pane = %+v, %v; want heuristic %s", status, p, err, want)
		}
		listed, _ := b.List(ctx)
		if len(listed) != 1 || listed[0].Heuristic != want {
			t.Errorf("status %q: List = %+v", status, listed)
		}
	}
}

func TestAttachFocusesAndReturnsTheSessionCommand(t *testing.T) {
	fake := startFakeHerdr(t)
	b := herdr.New(herdr.Options{Session: "rig", Binary: "herdr", Socket: fake.path})
	id := mustSpawn(t, b, spec(t, "demo", "api", "cat"))
	cmd, err := b.Attach(ctx, id)
	if err != nil || !reflect.DeepEqual(cmd, []string{"herdr", "session", "attach", "rig"}) {
		t.Errorf("Attach = %v, %v", cmd, err)
	}
	if fake.callCount("workspace.focus") != 1 || fake.callCount("tab.focus") != 1 {
		t.Error("Attach should bring the pane to the front for the human")
	}
}

func TestReadModes(t *testing.T) {
	fake := startFakeHerdr(t)
	b := backendFor(fake)
	id := mustSpawn(t, b, spec(t, "demo", "api"))
	for _, line := range []string{"one", "two", "three"} {
		b.SendText(ctx, id, line)
		b.Submit(ctx, id)
	}
	if out, _ := b.Read(ctx, id, 0); !strings.Contains(out, "one") {
		t.Errorf("visible = %q", out)
	}
	if out, _ := b.Read(ctx, id, 2); strings.Contains(out, "one") || !strings.Contains(out, "three") {
		t.Errorf("last two lines = %q", out)
	}
}

func TestErrorsFromHerdrBecomeTheRightSentinels(t *testing.T) {
	fake := startFakeHerdr(t)
	b := backendFor(fake)
	if _, err := b.Get(ctx, "w9:p9"); !errors.Is(err, terminal.ErrNotFound) {
		t.Errorf("an unknown pane = %v", err)
	}

	// A server that is not there is unavailable, with a message naming the problem.
	gone := herdr.New(herdr.Options{Session: "rig", Socket: filepath.Join(filepath.Dir(shortSocketPath(t)), "missing.sock")})
	_, err := gone.List(ctx)
	if !errors.Is(err, terminal.ErrUnavailable) || !strings.Contains(err.Error(), "not running") {
		t.Errorf("no server: %v, want ErrUnavailable saying the session is not running", err)
	}

	// A server that stops mid-session.
	fake.stop()
	if _, err := b.List(ctx); !errors.Is(err, terminal.ErrUnavailable) {
		t.Errorf("after the server stopped: %v", err)
	}
}

func TestACancelledContextIsNotReportedAsUnavailable(t *testing.T) {
	fake := startFakeHerdr(t)
	b := backendFor(fake)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.List(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestConcurrentSpawnsOfOneTicketShareOneWorkspace(t *testing.T) {
	fake := startFakeHerdr(t)
	b := backendFor(fake)
	var wg sync.WaitGroup
	for _, svc := range []string{"api", "web", "worker", "db", "cache", "queue"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Spawn(ctx, spec(t, "demo", svc, "cat")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fake.callCount("workspace.create"); n != 1 {
		t.Errorf("workspace.create called %d times, want exactly 1", n)
	}
	if got := len(fake.snapshot()["demo"]); got != 6 {
		t.Errorf("demo has %d tabs, want 6", got)
	}
}

// fakeExecutable stands in for the herdr program: `session list` prints a canned table
// and `--session X server` records that it was started, then idles like a server.
func fakeExecutable(t *testing.T, socket string) (binary, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "invocations.log")
	binary = filepath.Join(dir, "herdr")
	script := `#!/bin/sh
case "$1" in
  session)
    echo "name status directory socket"
    echo "default running /cfg /cfg/herdr.sock"
    echo "rig stopped /cfg/sessions/rig ` + socket + `"
    ;;
  --session)
    echo "$$ $@" >> ` + log + `
    exec sleep 30
    ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		raw, _ := os.ReadFile(log)
		for _, line := range strings.Split(string(raw), "\n") {
			if fields := strings.Fields(line); len(fields) > 0 {
				exec.Command("kill", fields[0]).Run()
			}
		}
	})
	return binary, log
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReadyStartsTheServerOnceAndFindsItsSocket(t *testing.T) {
	socket := shortSocketPath(t)
	binary, log := fakeExecutable(t, socket)
	b := herdr.New(herdr.Options{Session: "rig", Binary: binary, StartTimeout: 5 * time.Second, PollInterval: 10 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- b.Ready(ctx) }()

	waitForFile(t, log) // the backend ran `herdr --session rig server`
	startFakeHerdrAt(t, socket)
	if err := <-done; err != nil {
		t.Fatalf("Ready: %v", err)
	}
	raw, _ := os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], "--session rig server") {
		t.Errorf("server invocations = %q, want exactly one `--session rig server`", raw)
	}

	if err := b.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(log)
	if n := strings.Count(string(raw), "server"); n != 1 {
		t.Errorf("a second Ready started another server (%d starts)", n)
	}
	// The socket came from `session list`, so operations now work without options.
	if _, err := b.Spawn(ctx, spec(t, "demo", "api", "cat")); err != nil {
		t.Errorf("Spawn after Ready: %v", err)
	}
}

func TestReadyGivesUpWhenTheServerNeverComesUp(t *testing.T) {
	binary, _ := fakeExecutable(t, shortSocketPath(t))
	clock := testkit.NewFakeClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	b := herdr.New(herdr.Options{Session: "rig", Binary: binary, StartTimeout: 5 * time.Second, PollInterval: 100 * time.Millisecond, Clock: clock})

	result := make(chan error, 1)
	go func() { result <- b.Ready(ctx) }()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-result:
			if !errors.Is(err, terminal.ErrUnavailable) || !strings.Contains(err.Error(), "did not come up") {
				t.Fatalf("error = %v, want ErrUnavailable saying the server did not come up", err)
			}
			return
		case <-deadline:
			t.Fatal("Ready never gave up")
		default:
			clock.Advance(100 * time.Millisecond)
			time.Sleep(time.Millisecond)
		}
	}
}

func TestReadyExplainsAMissingHerdr(t *testing.T) {
	b := herdr.New(herdr.Options{Binary: "definitely-not-a-herdr-binary-xyz"})
	err := b.Ready(ctx)
	if !errors.Is(err, terminal.ErrUnavailable) || !strings.Contains(err.Error(), "not installed") || !strings.Contains(err.Error(), "herdr.dev") {
		t.Errorf("error = %v, want ErrUnavailable with an install hint", err)
	}
	if _, err := b.List(ctx); !errors.Is(err, terminal.ErrUnavailable) {
		t.Errorf("List without herdr = %v", err)
	}
}

func TestTheSocketIsDerivedFromTheDefaultSessionWhenTheSessionIsUnknown(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "herdr")
	os.WriteFile(binary, []byte("#!/bin/sh\necho 'name status directory socket'\necho 'default running /cfg /cfg/herdr.sock'\n"), 0o755)
	b := herdr.New(herdr.Options{Session: "brand-new", Binary: binary})
	// No server is listening, so listing fails, but the error must name the derived path.
	_, err := b.List(ctx)
	if !errors.Is(err, terminal.ErrUnavailable) || !strings.Contains(err.Error(), "/cfg/sessions/brand-new/herdr.sock") {
		t.Errorf("error = %v, want it to name /cfg/sessions/brand-new/herdr.sock", err)
	}
}
