package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/tui"
)

// twoHomes sets up a user with two homes, "work" and "home", registered by name, each
// with its own config; neither daemon runs. It returns the two configs.
func twoHomes(t *testing.T) (work, home string) {
	t.Helper()
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("RIG_HOME", "")
	t.Setenv("RIG_CONFIG", "")
	t.Setenv("RIG_HOME_NAME", "")
	mk := func(name string) string {
		dir, err := os.MkdirTemp("", "rh")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		cfg := filepath.Join(dir, "config.json")
		os.WriteFile(cfg, []byte(fmt.Sprintf(`{"paths": {"home": %q}}`, filepath.Join(dir, "home"))), 0o600)
		if _, stderr, code := rig(t, deps{}, "homes", "add", name, cfg); code != 0 {
			t.Fatalf("homes add %s: %s", name, stderr)
		}
		return cfg
	}
	return mk("work"), mk("home")
}

// startDaemon runs one home's daemon in this process until the test ends.
func startDaemon(t *testing.T, config string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		empty := func(context.Context, ...string) (string, string, int, error) {
			return "", "item could not be found", 44, nil
		}
		d := deps{backend: testkit.NewFakeTerminal(), docker: testkit.NewFakeDocker(), keychain: empty, gh: testkit.NewFakeGH()}
		done <- run(ctx, []string{"--config", config, "daemon", "run"}, &bytes.Buffer{}, &bytes.Buffer{}, d)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not exit")
		}
	})
	for i := 0; i < 200; i++ {
		if _, _, code := rig(t, deps{}, "--config", config, "daemon", "status"); code == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon did not start")
}

// noSpawn fails the test if a command tries to start a daemon.
func noSpawn(t *testing.T) deps {
	return deps{spawner: func(context.Context) error { t.Error("a daemon was started"); return fmt.Errorf("no") }}
}

func TestAHomeIsChosenByNameFlagOrEnvironment(t *testing.T) {
	work, _ := twoHomes(t)
	startDaemon(t, work)

	if _, _, code := rig(t, noSpawn(t), "-H", "work", "daemon", "status"); code != 0 {
		t.Errorf("-H work: exit %d, want its running daemon", code)
	}
	if _, _, code := rig(t, noSpawn(t), "--home", "home", "daemon", "status"); code != 4 {
		t.Errorf("--home home: exit %d, want 4 (its daemon is not running)", code)
	}
	t.Setenv("RIG_HOME_NAME", "work")
	if _, _, code := rig(t, noSpawn(t), "daemon", "status"); code != 0 {
		t.Errorf("RIG_HOME_NAME=work: exit %d", code)
	}
	if _, _, code := rig(t, noSpawn(t), "-H", "home", "daemon", "status"); code != 4 {
		t.Errorf("the flag must beat RIG_HOME_NAME: exit %d", code)
	}
}

func TestTheOrderOfChoicesAndTheMistakes(t *testing.T) {
	work, home := twoHomes(t)
	startDaemon(t, work)

	t.Setenv("RIG_CONFIG", home)
	if _, _, code := rig(t, noSpawn(t), "-H", "work", "daemon", "status"); code != 0 {
		t.Errorf("--home must beat RIG_CONFIG: exit %d", code)
	}
	t.Setenv("RIG_HOME_NAME", "work")
	if _, _, code := rig(t, noSpawn(t), "daemon", "status"); code != 4 {
		t.Errorf("RIG_CONFIG must beat RIG_HOME_NAME: exit %d", code)
	}
	_, stderr, code := rig(t, noSpawn(t), "--config", work, "-H", "work", "daemon", "status")
	if code != 2 || !strings.Contains(stderr, "--config") {
		t.Errorf("--config with --home: exit %d, %q", code, stderr)
	}
	_, stderr, code = rig(t, noSpawn(t), "-H", "nope", "daemon", "status")
	if code != 2 || !strings.Contains(stderr, "home") || !strings.Contains(stderr, "work") {
		t.Errorf("an unknown home: exit %d, %q; want a usage error naming the known homes", code, stderr)
	}
}

func TestAddingAndRemovingHomes(t *testing.T) {
	work, _ := twoHomes(t)
	dir := filepath.Dir(work)
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"paths": {"hme": "x"}}`), 0o600)
	for name, args := range map[string][]string{
		"a missing config":  {"homes", "add", "x", filepath.Join(dir, "none.json")},
		"an invalid config": {"homes", "add", "x", bad},
		"a bad name":        {"homes", "add", "a b", work},
		"a taken name":      {"homes", "add", "work", work},
		"default":           {"homes", "add", "default", work},
	} {
		if _, stderr, code := rig(t, noSpawn(t), args...); code == 0 {
			t.Errorf("%s was accepted: %q", name, stderr)
		}
	}
	if _, _, code := rig(t, noSpawn(t), "homes", "remove", "work"); code != 0 {
		t.Fatal("remove failed")
	}
	if _, _, code := rig(t, noSpawn(t), "-H", "work", "daemon", "status"); code != 2 {
		t.Errorf("a removed home can still be chosen: exit %d", code)
	}
	if _, err := os.Stat(work); err != nil {
		t.Error("removing a home deleted its config")
	}
	if _, _, code := rig(t, noSpawn(t), "homes", "remove", "work"); code == 0 {
		t.Error("removing an unknown home succeeded")
	}
}

func TestHomesShowsEachHomeWithoutStartingAnyDaemon(t *testing.T) {
	work, home := twoHomes(t)
	startDaemon(t, work)
	stdout, stderr, code := rig(t, noSpawn(t), "homes")
	if code != 0 {
		t.Fatalf("homes: %d %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	row := func(name string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, name+" ") {
				return l
			}
		}
		return ""
	}
	if w := row("work"); !strings.Contains(w, "running") || !strings.Contains(w, filepath.Dir(work)) {
		t.Errorf("work row %q in\n%s", w, stdout)
	}
	if h := row("home"); !strings.Contains(h, "stopped") || !strings.Contains(h, filepath.Dir(home)) {
		t.Errorf("home row %q in\n%s", h, stdout)
	}
	if row("default") != "" {
		t.Error("default is listed although its config does not exist")
	}

	stdout, _, _ = rig(t, noSpawn(t), "homes", "--json")
	var got []struct {
		Name    string `json:"name"`
		Config  string `json:"config"`
		Home    string `json:"home"`
		Running bool   `json:"running"`
		Tickets *int   `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || len(got) != 2 || got[0].Name != "home" || got[1].Name != "work" || !got[1].Running || got[1].Tickets == nil || got[0].Running {
		t.Errorf("homes --json = %s (%v)", stdout, err)
	}
}

func TestListAllShowsEveryRunningHomesTickets(t *testing.T) {
	work, _ := twoHomes(t)
	startDaemon(t, work)
	stdout, stderr, code := rig(t, noSpawn(t), "list", "--all")
	if code != 0 || !strings.Contains(stdout, "work") || !strings.Contains(stdout, "no tickets") || !strings.Contains(stdout, "home: daemon not running") {
		t.Errorf("list --all: %d\n%s\n%s", code, stdout, stderr)
	}
	stdout, _, _ = rig(t, noSpawn(t), "list", "--all", "--json")
	var got []struct {
		Home    string            `json:"home"`
		Running bool              `json:"running"`
		Tickets []json.RawMessage `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || len(got) != 2 {
		t.Errorf("list --all --json = %s (%v)", stdout, err)
	}
}

// tuiDeps is a person at a terminal whose TUI and picker are recorded instead of shown.
type tuiRecord struct {
	opts   []tui.Options
	reach  []error // whether each run's source reached a running daemon, asked during the run
	picks  [][]tui.HomeRow
	choose []string // what the picker answers, in order
	runErr []error  // what each TUI run returns, in order
}

func (r *tuiRecord) deps(t *testing.T) deps {
	d := deps{spawner: func(context.Context) error { return fmt.Errorf("not running") }}
	d.isTerminal = func() bool { return true }
	d.runTUI = func(_ context.Context, o tui.Options) error {
		r.opts = append(r.opts, o)
		_, err := o.Source.Tickets(context.Background())
		r.reach = append(r.reach, err)
		if len(r.runErr) >= len(r.opts) {
			return r.runErr[len(r.opts)-1]
		}
		return nil
	}
	d.pickHome = func(_ context.Context, rows []tui.HomeRow) (string, error) {
		r.picks = append(r.picks, rows)
		if len(r.choose) < len(r.picks) {
			t.Fatal("the picker was shown more often than expected")
		}
		return r.choose[len(r.picks)-1], nil
	}
	return d
}

func TestBareRigOffersThePickerAndOpensTheChosenHome(t *testing.T) {
	work, _ := twoHomes(t)
	startDaemon(t, work)
	r := &tuiRecord{choose: []string{"work", "home"}, runErr: []error{tui.ErrSwitchHome}}
	if _, stderr, code := rig(t, r.deps(t)); code != 0 {
		t.Fatalf("rig: %d %s", code, stderr)
	}
	if len(r.picks) != 2 || len(r.opts) != 2 {
		t.Fatalf("picks %d, runs %d; want the picker again after H", len(r.picks), len(r.opts))
	}
	rows := r.picks[0]
	if len(rows) != 2 || rows[0].Name != "home" || rows[0].Status != "stopped" || rows[1].Name != "work" || !strings.Contains(rows[1].Status, "running") {
		t.Errorf("rows = %+v", rows)
	}
	if o := r.opts[0]; o.Home != "work" || !o.CanSwitch {
		t.Errorf("first run: home %q, can switch %v", o.Home, o.CanSwitch)
	}
	if r.reach[0] != nil {
		t.Errorf("the TUI's source does not reach work's daemon: %v", r.reach[0])
	}
	if r.opts[1].Home != "home" || r.reach[1] == nil {
		t.Errorf("after switching: home %q, reached %v (home's daemon is not running)", r.opts[1].Home, r.reach[1])
	}

	quit := &tuiRecord{choose: []string{""}}
	if _, _, code := rig(t, quit.deps(t)); code != 0 || len(quit.opts) != 0 {
		t.Errorf("quitting the picker: exit %d, TUI runs %d", code, len(quit.opts))
	}
}

func TestAChosenHomeOrADeepLinkSkipsThePicker(t *testing.T) {
	work, _ := twoHomes(t)
	startDaemon(t, work)
	r := &tuiRecord{}
	if _, stderr, code := rig(t, r.deps(t), "-H", "work", "-t", "demo-1", "--tab", "diff"); code != 0 {
		t.Fatalf("rig: %d %s", code, stderr)
	}
	if len(r.opts) != 1 {
		t.Fatalf("runs %d", len(r.opts))
	}
	o := r.opts[0]
	if o.Home != "work" || o.CanSwitch || o.Open == nil || o.Open.Ticket != "demo-1" || o.Open.Tab != tui.TabDiff {
		t.Errorf("opts: home %q, switch %v, open %+v", o.Home, o.CanSwitch, o.Open)
	}
	t.Setenv("RIG_HOME_NAME", "work")
	r = &tuiRecord{}
	rig(t, r.deps(t), "-t", "demo-1")
	if len(r.opts) != 1 || r.opts[0].Home != "work" || r.opts[0].Open.Tab != tui.TabPlan {
		t.Errorf("RIG_HOME_NAME: %+v", r.opts)
	}
	for _, args := range [][]string{{"-H", "work", "--tab", "nope", "-t", "x"}, {"-H", "work", "--tab", "diff"}} {
		if _, _, code := rig(t, (&tuiRecord{}).deps(t), args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
