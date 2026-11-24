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
