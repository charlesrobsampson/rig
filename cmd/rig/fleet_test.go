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

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/tui"
)

// twoVessels sets up a user with two vessels, "work" and "home", registered by name, each
// with its own config; neither daemon runs. It returns the two configs.
func twoVessels(t *testing.T) (work, home string) {
	t.Helper()
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("RIG_HOME", "")
	t.Setenv("RIG_CONFIG", "")
	t.Setenv("RIG_VESSEL", "")
	mk := func(name string) string {
		dir, err := os.MkdirTemp("", "rh")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		cfg := filepath.Join(dir, "config.json")
		os.WriteFile(cfg, []byte(fmt.Sprintf(`{"paths": {"home": %q}}`, filepath.Join(dir, "home"))), 0o600)
		if _, stderr, code := rig(t, deps{}, "fleet", "add", name, cfg); code != 0 {
			t.Fatalf("fleet add %s: %s", name, stderr)
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
		case <-time.After(testkit.WaitLimit):
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

func TestAVesselIsChosenByNameFlagOrEnvironment(t *testing.T) {
	work, _ := twoVessels(t)
	startDaemon(t, work)

	if _, _, code := rig(t, noSpawn(t), "-V", "work", "daemon", "status"); code != 0 {
		t.Errorf("-H work: exit %d, want its running daemon", code)
	}
	if _, _, code := rig(t, noSpawn(t), "--vessel", "home", "daemon", "status"); code != 4 {
		t.Errorf("--vessel home: exit %d, want 4 (its daemon is not running)", code)
	}
	t.Setenv("RIG_VESSEL", "work")
	if _, _, code := rig(t, noSpawn(t), "daemon", "status"); code != 0 {
		t.Errorf("RIG_VESSEL=work: exit %d", code)
	}
	if _, _, code := rig(t, noSpawn(t), "-V", "home", "daemon", "status"); code != 4 {
		t.Errorf("the flag must beat RIG_VESSEL: exit %d", code)
	}
}

func TestTheOrderOfChoicesAndTheMistakes(t *testing.T) {
	work, home := twoVessels(t)
	startDaemon(t, work)

	t.Setenv("RIG_CONFIG", home)
	if _, _, code := rig(t, noSpawn(t), "-V", "work", "daemon", "status"); code != 0 {
		t.Errorf("--vessel must beat RIG_CONFIG: exit %d", code)
	}
	t.Setenv("RIG_VESSEL", "work")
	if _, _, code := rig(t, noSpawn(t), "daemon", "status"); code != 4 {
		t.Errorf("RIG_CONFIG must beat RIG_VESSEL: exit %d", code)
	}
	_, stderr, code := rig(t, noSpawn(t), "--config", work, "-V", "work", "daemon", "status")
	if code != 2 || !strings.Contains(stderr, "--config") {
		t.Errorf("--config with --vessel: exit %d, %q", code, stderr)
	}
	_, stderr, code = rig(t, noSpawn(t), "-V", "nope", "daemon", "status")
	if code != 2 || !strings.Contains(stderr, "vessel") || !strings.Contains(stderr, "work") {
		t.Errorf("an unknown vessel: exit %d, %q; want a usage error naming the known vessels", code, stderr)
	}
}

func TestAddingAndRemovingVessels(t *testing.T) {
	work, _ := twoVessels(t)
	dir := filepath.Dir(work)
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"paths": {"hme": "x"}}`), 0o600)
	for name, args := range map[string][]string{
		"a missing config":  {"fleet", "add", "x", filepath.Join(dir, "none.json")},
		"an invalid config": {"fleet", "add", "x", bad},
		"a bad name":        {"fleet", "add", "a b", work},
		"a taken name":      {"fleet", "add", "work", work},
		"default":           {"fleet", "add", "default", work},
	} {
		if _, stderr, code := rig(t, noSpawn(t), args...); code == 0 {
			t.Errorf("%s was accepted: %q", name, stderr)
		}
	}
	if _, _, code := rig(t, noSpawn(t), "fleet", "remove", "work"); code != 0 {
		t.Fatal("remove failed")
	}
	if _, _, code := rig(t, noSpawn(t), "-V", "work", "daemon", "status"); code != 2 {
		t.Errorf("a removed vessel can still be chosen: exit %d", code)
	}
	if _, err := os.Stat(work); err != nil {
		t.Error("removing a vessel deleted its config")
	}
	if _, _, code := rig(t, noSpawn(t), "fleet", "remove", "work"); code == 0 {
		t.Error("removing an unknown vessel succeeded")
	}
}

func TestFleetShowsEachVesselWithoutStartingAnyDaemon(t *testing.T) {
	work, home := twoVessels(t)
	startDaemon(t, work)
	stdout, stderr, code := rig(t, noSpawn(t), "fleet")
	if code != 0 {
		t.Fatalf("fleet: %d %s", code, stderr)
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

	stdout, _, _ = rig(t, noSpawn(t), "fleet", "--json")
	var got []struct {
		Name    string `json:"name"`
		Config  string `json:"config"`
		Home    string `json:"home"`
		Running bool   `json:"running"`
		Tickets *int   `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || len(got) != 2 || got[0].Name != "home" || got[1].Name != "work" || !got[1].Running || got[1].Tickets == nil || got[0].Running {
		t.Errorf("fleet --json = %s (%v)", stdout, err)
	}
}

func TestListAllShowsEveryRunningVesselsTickets(t *testing.T) {
	work, _ := twoVessels(t)
	startDaemon(t, work)
	stdout, stderr, code := rig(t, noSpawn(t), "list", "--all")
	if code != 0 || !strings.Contains(stdout, "work") || !strings.Contains(stdout, "no tickets") || !strings.Contains(stdout, "home: daemon not running") {
		t.Errorf("list --all: %d\n%s\n%s", code, stdout, stderr)
	}
	stdout, _, _ = rig(t, noSpawn(t), "list", "--all", "--json")
	var got []struct {
		Vessel  string            `json:"vessel"`
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
	picks  [][]tui.VesselRow
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
	d.pickVessel = func(_ context.Context, rows []tui.VesselRow) (string, error) {
		r.picks = append(r.picks, rows)
		if len(r.choose) < len(r.picks) {
			t.Fatal("the picker was shown more often than expected")
		}
		return r.choose[len(r.picks)-1], nil
	}
	return d
}

func TestBareRigOffersThePickerAndOpensTheChosenVessel(t *testing.T) {
	work, _ := twoVessels(t)
	startDaemon(t, work)
	r := &tuiRecord{choose: []string{"work", "home"}, runErr: []error{tui.ErrSwitchVessel}}
	if _, stderr, code := rig(t, r.deps(t)); code != 0 {
		t.Fatalf("rig: %d %s", code, stderr)
	}
	if len(r.picks) != 2 || len(r.opts) != 2 {
		t.Fatalf("picks %d, runs %d; want the picker again after F", len(r.picks), len(r.opts))
	}
	rows := r.picks[0]
	if len(rows) != 2 || rows[0].Name != "home" || rows[0].Status != "stopped" || rows[1].Name != "work" || !strings.Contains(rows[1].Status, "running") {
		t.Errorf("rows = %+v", rows)
	}
	if o := r.opts[0]; o.Vessel != "work" || !o.CanSwitch {
		t.Errorf("first run: home %q, can switch %v", o.Vessel, o.CanSwitch)
	}
	if r.reach[0] != nil {
		t.Errorf("the TUI's source does not reach work's daemon: %v", r.reach[0])
	}
	if r.opts[1].Vessel != "home" || r.reach[1] == nil {
		t.Errorf("after switching: home %q, reached %v (home's daemon is not running)", r.opts[1].Vessel, r.reach[1])
	}

	quit := &tuiRecord{choose: []string{""}}
	if _, _, code := rig(t, quit.deps(t)); code != 0 || len(quit.opts) != 0 {
		t.Errorf("quitting the picker: exit %d, TUI runs %d", code, len(quit.opts))
	}
}

func TestAChosenVesselOrADeepLinkSkipsThePicker(t *testing.T) {
	work, _ := twoVessels(t)
	startDaemon(t, work)
	r := &tuiRecord{}
	if _, stderr, code := rig(t, r.deps(t), "-V", "work", "-t", "demo-1", "--tab", "diff"); code != 0 {
		t.Fatalf("rig: %d %s", code, stderr)
	}
	if len(r.opts) != 1 {
		t.Fatalf("runs %d", len(r.opts))
	}
	o := r.opts[0]
	if o.Vessel != "work" || !o.CanSwitch || o.Open == nil || o.Open.Ticket != "demo-1" || o.Open.Tab != tui.TabDiff {
		t.Errorf("opts: home %q, switch %v, open %+v", o.Vessel, o.CanSwitch, o.Open)
	}
	t.Setenv("RIG_VESSEL", "work")
	r = &tuiRecord{}
	rig(t, r.deps(t), "-t", "demo-1")
	if len(r.opts) != 1 || r.opts[0].Vessel != "work" || r.opts[0].Open.Tab != tui.TabPlan {
		t.Errorf("RIG_VESSEL: %+v", r.opts)
	}
	for _, args := range [][]string{{"-V", "work", "--tab", "nope", "-t", "x"}, {"-V", "work", "--tab", "diff"}} {
		if _, _, code := rig(t, (&tuiRecord{}).deps(t), args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestFOpensTheFleetEvenFromAVesselChosenUpFront(t *testing.T) {
	work, _ := twoVessels(t)
	startDaemon(t, work)
	r := &tuiRecord{choose: []string{"home"}, runErr: []error{tui.ErrSwitchVessel}}
	if _, stderr, code := rig(t, r.deps(t), "-V", "work", "-t", "demo-1"); code != 0 {
		t.Fatalf("rig: %d %s", code, stderr)
	}
	if len(r.opts) != 2 || len(r.picks) != 1 || r.opts[1].Vessel != "home" || r.opts[1].Open != nil {
		t.Errorf("runs %d, picks %d, second %+v", len(r.opts), len(r.picks), r.opts)
	}

	t.Setenv("HOME", t.TempDir()) // a machine with one vessel: nothing to go back to
	if _, stderr, code := rig(t, noSpawn(t), "fleet", "add", "only", work); code != 0 {
		t.Fatal(stderr)
	}
	r = &tuiRecord{}
	rig(t, r.deps(t), "-V", "only")
	if len(r.opts) != 1 || r.opts[0].CanSwitch {
		t.Errorf("one vessel: %+v", r.opts)
	}
}

// ticketIn makes a ticket on a vessel.
func ticketIn(t *testing.T, vessel, id string) {
	t.Helper()
	repo := testkit.NewRepo(t)
	if _, stderr, code := rig(t, deps{}, "-V", vessel, "new", "--id", id, "--title", "T "+id, "--service", "svc1="+repo.Dir); code != 0 {
		t.Fatalf("new %s/%s: %s", vessel, id, stderr)
	}
}

func TestWaitWatchesTicketsOnSeveralVesselsAndEndsOnTheFirst(t *testing.T) {
	work, home := twoVessels(t)
	startDaemon(t, work)
	startDaemon(t, home)
	ticketIn(t, "work", "a")
	ticketIn(t, "home", "b")

	stdout, stderr, code := rig(t, noSpawn(t), "wait", "work/a", "home/b", "--phase", "plan", "--timeout", "5s")
	if code != 0 || !strings.Contains(stdout, "work/a: phase is plan") {
		t.Errorf("wait: %d %q %q", code, stdout, stderr)
	}
	stdout, _, _ = rig(t, noSpawn(t), "wait", "home/b", "work/a", "--phase", "plan", "--json")
	var got struct {
		Ticket  string `json:"ticket"`
		Matched string `json:"matched"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || got.Ticket != "home/b" || got.Matched != "phase" {
		t.Errorf("--json: %q (%v)", stdout, err)
	}
	if _, _, code := rig(t, noSpawn(t), "wait", "work/a", "home/b", "--phase", "push", "--timeout", "1s"); code != 1 {
		t.Errorf("nothing matching: exit %d, want the timeout", code)
	}
	if _, _, code := rig(t, noSpawn(t), "wait", "sea/a", "--phase", "plan"); code != 2 {
		t.Errorf("an unknown vessel: exit %d", code)
	}
}

func TestAVesselQualifiedTicketWorksWhereAnIdDoesAndHintsNameTheVessel(t *testing.T) {
	work, home := twoVessels(t)
	startDaemon(t, work)
	startDaemon(t, home)
	repo := testkit.NewRepo(t)
	stdout, _, code := rig(t, noSpawn(t), "-V", "work", "new", "--id", "a", "--title", "T", "--service", "svc1="+repo.Dir)
	if code != 0 || !strings.Contains(stdout, "rig -V work start a") {
		t.Errorf("new's hint: %q", stdout)
	}
	if out, stderr, code := rig(t, noSpawn(t), "gates", "work/a"); code != 0 {
		t.Errorf("gates work/a: %d %q %q", code, out, stderr)
	}
	if _, stderr, code := rig(t, noSpawn(t), "-V", "home", "gates", "work/a"); code != 2 || !strings.Contains(stderr, "work") {
		t.Errorf("a ticket of another vessel than -V: %d %q", code, stderr)
	}
	_, stderr, _ := rig(t, noSpawn(t), "push", "work/a")
	if !strings.Contains(stderr, "rig -V work phase a push") {
		t.Errorf("the daemon's hint does not name the vessel: %q", stderr)
	}
}

func TestTheFleetShowsWhereEachVesselsTicketsStand(t *testing.T) {
	work, _ := twoVessels(t)
	startDaemon(t, work)
	ticketIn(t, "work", "a")
	stdout, _, _ := rig(t, noSpawn(t), "fleet")
	if !strings.Contains(stdout, "1 (1 running)") {
		t.Errorf("fleet:\n%s", stdout)
	}
	if out, _, code := rig(t, noSpawn(t), "watch", "--help"); code != 0 || !strings.Contains(out, "watch") {
		t.Errorf("rig watch: %d", code)
	}
	if _, _, code := rig(t, noSpawn(t), "events", "--help"); code != 0 {
		t.Error("rig events no longer works")
	}
}

func TestTheDefaultVesselIsLeftOutUntilItHoldsTickets(t *testing.T) {
	work, _ := twoVessels(t)
	user := os.Getenv("HOME")
	machine := filepath.Join(user, ".config", "rig", "config.json")
	os.MkdirAll(filepath.Dir(machine), 0o700)
	defHome := filepath.Join(user, "defhome")
	os.WriteFile(machine, []byte(fmt.Sprintf(`{"paths": {"home": %q}}`, defHome)), 0o600)
	_ = work

	out, stderr, code := rig(t, noSpawn(t), "fleet")
	if code != 0 || strings.Contains(out, "default") || !strings.Contains(out, "work") {
		t.Fatalf("fleet without tickets: %d %q %q", code, out, stderr)
	}
	if _, stderr, code := rig(t, noSpawn(t), "--vessel", "default", "config", "show"); code != 0 {
		t.Errorf("default is still choosable by name: %s", stderr)
	}
	dir := filepath.Join(defHome, "tickets", "t1")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "ticket.yaml"), []byte("id: t1\n"), 0o600)
	out, _, _ = rig(t, noSpawn(t), "fleet")
	if !strings.Contains(out, "default") {
		t.Errorf("fleet with a ticket: %q", out)
	}
}

func TestAnEmptyDefaultVesselIsNoReasonForAPicker(t *testing.T) {
	work, _ := twoVessels(t)
	if _, stderr, code := rig(t, deps{}, "fleet", "remove", "home"); code != 0 {
		t.Fatal(stderr)
	}
	machine := filepath.Join(os.Getenv("HOME"), ".config", "rig", "config.json")
	os.MkdirAll(filepath.Dir(machine), 0o700)
	os.WriteFile(machine, []byte(fmt.Sprintf(`{"paths": {"home": %q}}`, filepath.Join(os.Getenv("HOME"), "defhome"))), 0o600)
	startDaemon(t, work)

	r := &tuiRecord{}
	if _, stderr, code := rig(t, r.deps(t)); code != 0 {
		t.Fatalf("rig: %d %s", code, stderr)
	}
	if len(r.picks) != 0 || len(r.opts) != 1 || r.opts[0].Vessel != "work" || r.opts[0].CanSwitch {
		t.Errorf("picks %d, runs %+v; want no picker, work, no switching", len(r.picks), r.opts)
	}
	r = &tuiRecord{}
	if _, stderr, code := rig(t, r.deps(t), "-V", "work"); code != 0 || len(r.opts) != 1 || r.opts[0].CanSwitch {
		t.Errorf("-V work: %d %s %+v", code, stderr, r.opts)
	}
}

func TestFleetWarnsOnlyAboutTheDaemonsItListsAndNamesTheVessel(t *testing.T) {
	work, _ := twoVessels(t)
	user := os.Getenv("HOME")
	machine := filepath.Join(user, ".config", "rig", "config.json")
	os.MkdirAll(filepath.Dir(machine), 0o700)
	// a short path: a unix socket path is limited to 104 bytes on macOS
	defHome, err := os.MkdirTemp("", "rh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(defHome) })
	os.WriteFile(machine, []byte(fmt.Sprintf(`{"paths": {"home": %q}}`, defHome)), 0o600)
	old := version
	version = "1.0.0"
	t.Cleanup(func() { version = old })
	startDaemon(t, machine) // the default vessel's daemon runs but has no tickets, so fleet does not list it
	d := noSpawn(t)
	d.version = "2.0.0"

	out, stderr, code := rig(t, d, "fleet")
	if code != 0 || strings.Contains(out, "default") || strings.Contains(stderr, "daemon is version") {
		t.Fatalf("fleet: %d %q %q; want no warning for a daemon it does not list", code, out, stderr)
	}
	startDaemon(t, work)
	_, stderr, code = rig(t, d, "fleet")
	if code != 0 || strings.Count(stderr, "daemon is version") != 1 || !strings.Contains(stderr, `run "rig -V work daemon restart"`) {
		t.Errorf("fleet with a stale listed daemon: %d %q", code, stderr)
	}
}

func TestVesselSpendSumsEveryTicketAndThisLocalMonths(t *testing.T) {
	usd := func(v float64) *float64 { return &v }
	at := func(y int, m time.Month, d int) *time.Time {
		v := time.Date(y, m, d, 12, 0, 0, 0, time.Local)
		return &v
	}
	now := time.Date(2026, time.November, 30, 23, 0, 0, 0, time.Local)
	tickets := []api.TicketSummary{
		{ID: "done-old", Phase: domain.PhaseDone, CostUSD: usd(10), CreatedAt: at(2026, time.October, 31)},
		{ID: "done-now", Phase: domain.PhaseDone, CostUSD: usd(2.25), CreatedAt: at(2026, time.November, 1)},
		{ID: "running", CostUSD: usd(0.1), CreatedAt: at(2026, time.November, 30)},
		{ID: "no-cost", CreatedAt: at(2026, time.November, 2)},
		{ID: "last-year", CostUSD: usd(1), CreatedAt: at(2025, time.November, 5)},
	}
	total, month := vesselSpend(tickets, now)
	if total != 13.35 || month != 2.35 {
		t.Errorf("spend = %v, month %v; want 13.35, 2.35", total, month)
	}
	if total, month = vesselSpend(nil, now); total != 0 || month != 0 {
		t.Errorf("no tickets: %v %v", total, month)
	}
}

func TestFleetShowsSpendPerVesselRunningOrNot(t *testing.T) {
	work, home := twoVessels(t)
	startDaemon(t, work)
	ticketIn(t, "work", "a")
	ticketIn(t, "work", "b")
	startDaemon(t, home)
	ticketIn(t, "home", "c")
	// home's daemon is stopped again, so its ticket is read from the state file
	if _, stderr, code := rig(t, deps{}, "--config", home, "daemon", "stop"); code != 0 {
		t.Fatalf("daemon stop: %s", stderr)
	}
	for i := 0; ; i++ {
		if _, _, code := rig(t, noSpawn(t), "--config", home, "daemon", "status"); code != 0 {
			break
		}
		if i > 500 {
			t.Fatal("home's daemon did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}

	setCost := func(cfg, id string, usd float64, phase string) {
		g, err := config.LoadGlobal(config.GlobalOptions{Path: cfg})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(g.TicketsDir(), id, "state.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var st map[string]any
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		st["phase"] = phase
		for _, s := range st["services"].(map[string]any) {
			s.(map[string]any)["agent"] = map[string]any{"state": "idle", "cost": map[string]any{"usd": usd, "at": time.Now()}}
		}
		raw, _ = json.Marshal(st)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setCost(work, "a", 1.5, "done")
	setCost(work, "b", 10.84, "implement")
	setCost(home, "c", 3, "plan")

	stdout, stderr, code := rig(t, noSpawn(t), "fleet")
	if code != 0 {
		t.Fatalf("fleet: %d %s", code, stderr)
	}
	if !strings.Contains(stdout, "SPEND") || !strings.Contains(stdout, "$12.34") || !strings.Contains(stdout, "$3.00") {
		t.Errorf("fleet lacks the spend:\n%s", stdout)
	}
	stdout, _, _ = rig(t, noSpawn(t), "fleet", "--json")
	var got []struct {
		Name       string   `json:"name"`
		Running    bool     `json:"running"`
		Spend      *float64 `json:"spend_usd"`
		SpendMonth *float64 `json:"spend_month_usd"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || len(got) != 2 {
		t.Fatalf("fleet --json = %s (%v)", stdout, err)
	}
	for _, v := range got {
		if v.Running != (v.Name == "work") {
			t.Errorf("%s running = %v", v.Name, v.Running)
		}
		want := map[string]float64{"work": 12.34, "home": 3}[v.Name]
		if v.Spend == nil || v.SpendMonth == nil || *v.Spend < want-1e-9 || *v.Spend > want+1e-9 || *v.SpendMonth != *v.Spend {
			t.Errorf("%s (running %v): spend %v, month %v; want %v for both", v.Name, v.Running, v.Spend, v.SpendMonth, want)
		}
	}
}

func TestAVesselWithNoTicketsSpendsNothing(t *testing.T) {
	work, _ := twoVessels(t)
	startDaemon(t, work)
	stdout, _, _ := rig(t, noSpawn(t), "fleet")
	if !strings.Contains(stdout, "$0.00") {
		t.Errorf("fleet:\n%s", stdout)
	}
}
