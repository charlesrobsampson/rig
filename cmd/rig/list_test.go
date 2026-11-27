package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/client"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func addTicket(t *testing.T, home, id string, st *state.State) {
	t.Helper()
	dir := filepath.Join(home, "tickets", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "id: " + id + "\ntitle: Title of " + id + "\nservices:\n  - {name: backend, repo: /repos/b, branch: feat/" + id + "}\n  - {name: web, repo: /repos/w, branch: feat/" + id + "}\n"
	if err := os.WriteFile(filepath.Join(dir, "ticket.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if st != nil {
		st.TicketID = id
		mgr := state.NewManager(testkit.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)))
		if err := mgr.Create(filepath.Join(dir, "state.json"), *st); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListWithNoTickets(t *testing.T) {
	rigEnv(t)
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	deps := deps{spawner: d.spawn}

	stdout, stderr, code := rig(t, deps, "list")
	if code != 0 || !strings.Contains(stdout, "no tickets") {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if d.starts != 1 {
		t.Errorf("daemon started %d times; list should start it on demand", d.starts)
	}

	stdout, _, code = rig(t, deps, "list", "--json")
	var got api.TicketListResult
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || got.Tickets == nil || len(got.Tickets) != 0 {
		t.Errorf("--json: code %d, %q, %v; want an empty tickets array", code, stdout, err)
	}
}

func TestListShowsTicketsIncludingBrokenOnes(t *testing.T) {
	home := rigEnv(t)
	addTicket(t, home, "alpha", &state.State{
		Phase:    domain.PhaseImplement,
		Contract: state.Contract{Pending: true},
		Services: map[string]state.ServiceState{
			"backend": {Status: domain.ServiceRunning, HostPort: 41000},
			"web":     {Status: domain.ServiceProvisioned, HostPort: 41001},
		},
	})
	addTicket(t, home, "beta", &state.State{Phase: domain.PhasePlan})
	addTicket(t, home, "broken", nil) // no state.json

	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	stdout, stderr, code := rig(t, deps{spawner: d.spawn}, "list")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("output has %d lines, want a header and three tickets:\n%s", len(lines), stdout)
	}
	for _, col := range []string{"ID", "PHASE", "SERVICES", "PORTS", "TITLE"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q lacks column %s", lines[0], col)
		}
	}
	alpha := lines[1]
	for _, want := range []string{"alpha", "implement", "contract change pending", "backend:running", "web:provisioned", "41000", "41001", "Title of alpha"} {
		if !strings.Contains(alpha, want) {
			t.Errorf("alpha row %q lacks %q", alpha, want)
		}
	}
	if !strings.HasPrefix(lines[2], "beta") || !strings.Contains(lines[2], "plan") {
		t.Errorf("beta row = %q", lines[2])
	}
	if broken := lines[3]; !strings.HasPrefix(broken, "broken") || !strings.Contains(broken, "state.json not found") || !strings.Contains(broken, "!") {
		t.Errorf("broken row = %q; it should be listed with its problem", broken)
	}
}

func TestListJSONIsTheAPIPayload(t *testing.T) {
	home := rigEnv(t)
	addTicket(t, home, "alpha", &state.State{Phase: domain.PhaseTest})
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	deps := deps{spawner: d.spawn}

	stdout, _, code := rig(t, deps, "list", "--json")
	if code != 0 {
		t.Fatal("list --json failed")
	}
	var fromCLI api.TicketListResult
	if err := json.Unmarshal([]byte(stdout), &fromCLI); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout)
	}

	c, err := client.Connect(context.Background(), client.Options{SocketPath: filepath.Join(home, "rigd.sock"), Version: "test", Clock: clock.Real()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var fromAPI api.TicketListResult
	if err := c.Call(context.Background(), api.MethodTicketList, nil, &fromAPI); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromCLI, fromAPI) {
		t.Errorf("CLI JSON differs from the API payload:\n cli %+v\n api %+v", fromCLI, fromAPI)
	}
}

func TestAHandCreatedTicketAppearsOnTheNextList(t *testing.T) {
	home := rigEnv(t)
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	deps := deps{spawner: d.spawn}

	if stdout, _, _ := rig(t, deps, "list"); !strings.Contains(stdout, "no tickets") {
		t.Fatalf("expected no tickets, got %q", stdout)
	}
	addTicket(t, home, "handmade", &state.State{Phase: domain.PhaseReview})
	stdout, _, code := rig(t, deps, "list")
	if code != 0 || !strings.Contains(stdout, "handmade") || !strings.Contains(stdout, "review") {
		t.Errorf("code %d, stdout %q; a ticket created by hand should show up without a rescan", code, stdout)
	}
}

func TestListFailsClearlyWhenTheDaemonCannotStart(t *testing.T) {
	rigEnv(t)
	failing := deps{spawner: func(context.Context) error { return os.ErrPermission }}
	_, stderr, code := rig(t, failing, "list")
	if code != 4 || !strings.HasPrefix(stderr, "rig: ") {
		t.Errorf("code %d, stderr %q; want exit 4", code, stderr)
	}
}

func TestListShowsAPendingContractChangeAndADirectEditOfTheAgreedSection(t *testing.T) {
	home, d, repo := newEnv(t)
	other := testkit.NewRepo(t)
	if _, stderr, code := rig(t, d, "new", "--id", "duo", "--title", "Two", "--service", "api="+repo.Dir, "--service", "web="+other.Dir); code != 0 {
		t.Fatalf("new: %q", stderr)
	}
	statePath := filepath.Join(home, "tickets", "duo", "state.json")
	raw, _ := os.ReadFile(statePath)
	var st map[string]any
	json.Unmarshal(raw, &st)
	st["contract"] = map[string]any{"pending": true, "violation": "the agreed section of contract.md was edited directly"}
	out, _ := json.Marshal(st)
	os.WriteFile(statePath, out, 0o600)

	listing, _, code := rig(t, d, "list")
	if code != 0 || !strings.Contains(listing, "contract change pending") || !strings.Contains(listing, "contract edited outside rig") {
		t.Errorf("code %d, list:\n%s", code, listing)
	}
	jsonOut, _, _ := rig(t, d, "list", "--json")
	if !strings.Contains(jsonOut, `"contract_pending": true`) && !strings.Contains(jsonOut, `"contract_pending":true`) {
		t.Errorf("--json lacks contract_pending: %s", jsonOut)
	}
	if !strings.Contains(jsonOut, `contract_violation`) {
		t.Errorf("--json lacks contract_violation: %s", jsonOut)
	}
}

func TestListShowsAnAgentsPhaseRequest(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	statePath := filepath.Join(home, "tickets", "demo", "state.json")
	raw, _ := os.ReadFile(statePath)
	var st map[string]any
	json.Unmarshal(raw, &st)
	st["phase_request"] = map[string]any{"to": "implement", "service": "svc1", "at": "2026-11-19T00:00:00Z"}
	out, _ := json.Marshal(st)
	os.WriteFile(statePath, out, 0o600)

	if listing, _, code := rig(t, d, "list"); code != 0 || !strings.Contains(listing, "plan · svc1 asks for implement") {
		t.Errorf("code %d, list:\n%s", code, listing)
	}
}

func TestAnAgentsTransitionRequestRecordShowsOnList(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	if _, stderr, code := rig(t, d, "agent-event", `{"kind":"transition.request","data":{"to":"implement","reason":"plan ready"}}`); code != 0 {
		t.Fatalf("agent-event: %s", stderr)
	}
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "plan · svc1 asks for implement") {
		t.Errorf("list:\n%s", listing)
	}
	if _, stderr, code := rig(t, d, "approve", "demo", "plan_approval"); code != 0 {
		t.Fatal(stderr)
	}
	rig(t, d, "phase", "demo", "implement")
	if listing, _, _ := rig(t, d, "list"); strings.Contains(listing, "asks for") {
		t.Errorf("the request stayed after the move:\n%s", listing)
	}
}

func TestTheListFitsTheTerminalAndWideShowsEverything(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	long := "A title long enough that it cannot possibly fit on a narrow terminal next to everything else"
	if _, stderr, code := rig(t, d, "new", "--id", "longer", "--title", long, "--service", "svc1="+repo.Dir); code != 0 {
		t.Fatal(stderr)
	}
	d.termWidth = func() int { return 70 }
	out, _, code := rig(t, d, "list")
	if code != 0 {
		t.Fatal(out)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := ansi.StringWidth(line); w > 70 {
			t.Errorf("a %d-wide line on a 70-wide terminal: %q", w, line)
		}
	}
	if !strings.Contains(out, "…") || strings.Contains(out, long) {
		t.Errorf("the long title was not shortened:\n%s", out)
	}
	if strings.Contains(out, "PORTS") {
		t.Errorf("an empty PORTS column is shown:\n%s", out)
	}
	wide, _, _ := rig(t, d, "list", "--wide")
	if !strings.Contains(wide, long) || !strings.Contains(wide, "PORTS") {
		t.Errorf("--wide:\n%s", wide)
	}
	d.termWidth = func() int { return 0 } // not a terminal: scripts get everything
	if piped, _, _ := rig(t, d, "list"); !strings.Contains(piped, long) {
		t.Errorf("piped output was cut:\n%s", piped)
	}
}

func TestListNotesTicketsThatTouchTheSameFile(t *testing.T) {
	touch := func(service string, files ...string) []api.TouchedInfo {
		return []api.TouchedInfo{{Service: service, Files: files}}
	}
	tickets := []api.TicketSummary{
		{ID: "a", Phase: domain.PhaseImplement, Touched: touch("backend", "x.go", "a.go")},
		{ID: "b", Phase: domain.PhaseImplement, Touched: touch("backend", "x.go")},
		{ID: "c", Phase: domain.PhaseReview, Touched: touch("backend", "x.go")},
		{ID: "d", Phase: domain.PhaseDone, Touched: touch("backend", "x.go")},
		{ID: "e", Phase: domain.PhaseImplement, Touched: touch("web", "x.go")},
		{ID: "f", Phase: domain.PhaseImplement, Touched: touch("backend", "other.go")},
	}
	var out strings.Builder
	writeTicketTable(&out, tickets, time.Now(), 0) // the table reference time; nothing compares elapsed time
	lines := map[string]string{}
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	for id, want := range map[string]string{"a": "overlaps b · overlaps c", "b": "overlaps a · overlaps c", "c": "overlaps a · overlaps b"} {
		if !strings.Contains(lines[id], want) {
			t.Errorf("%s: %q lacks %q", id, lines[id], want)
		}
	}
	for _, id := range []string{"d", "e", "f"} {
		if strings.Contains(lines[id], "overlaps") {
			t.Errorf("%s notes an overlap: %q", id, lines[id])
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if strings.Contains(lines[id], "overlaps d") || strings.Contains(lines[id], "overlaps e") {
			t.Errorf("%s overlaps a done ticket or another service: %q", id, lines[id])
		}
	}
}

func TestListShowsAReviewThatWaitsForReady(t *testing.T) {
	tickets := []api.TicketSummary{
		{ID: "a", Phase: domain.PhaseImplement, Review: &api.ReviewSummary{Status: domain.ReviewAwaitingReady, Round: 2}},
		{ID: "b", Phase: domain.PhaseImplement, Review: &api.ReviewSummary{Status: domain.ReviewFixing, Round: 1}},
	}
	var out strings.Builder
	writeTicketTable(&out, tickets, time.Now(), 0) // the table reference time; nothing compares elapsed time
	if !strings.Contains(out.String(), "review waits for ready r2") || !strings.Contains(out.String(), "review fixing r1") {
		t.Errorf("list:\n%s", out.String())
	}
}

func TestListShowsAPassTheBranchHasMovedPast(t *testing.T) {
	tickets := []api.TicketSummary{
		{ID: "a", Phase: domain.PhaseImplement, Review: &api.ReviewSummary{Status: domain.ReviewPassed, Round: 2, Stale: true}},
		{ID: "b", Phase: domain.PhaseImplement, Review: &api.ReviewSummary{Status: domain.ReviewPassed, Round: 1}},
	}
	var out strings.Builder
	writeTicketTable(&out, tickets, time.Now(), 0) // the table reference time; nothing compares elapsed time
	if !strings.Contains(out.String(), "review passed r2 (new commits)") || strings.Contains(out.String(), "review passed r1 (") {
		t.Errorf("list:\n%s", out.String())
	}
}

func TestListShowsAStalledChecker(t *testing.T) {
	tickets := []api.TicketSummary{
		{ID: "a", Phase: domain.PhaseImplement, Review: &api.ReviewSummary{Status: domain.ReviewReviewing, Round: 1, Stalled: &api.ReviewStalled{Service: domain.TesterService}}},
		{ID: "b", Phase: domain.PhaseImplement, Review: &api.ReviewSummary{Status: domain.ReviewReviewing, Round: 1, Stalled: &api.ReviewStalled{Service: domain.ReviewerService}}},
	}
	var out strings.Builder
	writeTicketTable(&out, tickets, time.Now(), 0) // the table reference time; nothing compares elapsed time
	if !strings.Contains(out.String(), "review stalled (tester idle)") || !strings.Contains(out.String(), "review stalled (reviewer idle)") {
		t.Errorf("list:\n%s", out.String())
	}
}

func groupedFixture() []api.TicketSummary {
	cost := func(v float64) *float64 { return &v }
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	done := t0.Add(time.Hour)
	return []api.TicketSummary{
		{ID: "d1", Title: "old", Phase: domain.PhaseDone, CostUSD: cost(1.5), AgentSeconds: 600, CreatedAt: &t0, DoneAt: &done},
		{ID: "i1", Title: "one", Phase: domain.PhaseImplement, CostUSD: cost(2), AgentSeconds: 3000, CreatedAt: &t0},
		{ID: "i2", Title: "two", Phase: domain.PhaseImplement, CostUSD: cost(2.12), AgentSeconds: 1800, CreatedAt: &t0},
		{ID: "p1", Title: "fresh", Phase: domain.PhasePlan, CreatedAt: &t0},
	}
}

func TestListGroupCollapsesDoneToOneLineUnlessAllDone(t *testing.T) {
	now := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	var out strings.Builder
	writeGroupedTable(&out, groupedFixture(), now, 0, false)
	want := []string{"▾ plan (1)", "p1", "▾ implement (2)", "$4.12", "▸ done (1)", "$1.50", "all (4)", "$5.62"}
	last := -1
	for _, w := range want {
		i := strings.Index(out.String(), w)
		if i < last || i < 0 {
			t.Fatalf("lacks %q in order:\n%s", w, out.String())
		}
		last = i
	}
	if strings.Contains(out.String(), "d1") || !strings.Contains(out.String(), "i2") || !strings.Contains(out.String(), "AGENT") {
		t.Errorf("done must be one line, the others listed, with an AGENT column:\n%s", out.String())
	}
	out.Reset()
	writeGroupedTable(&out, groupedFixture(), now, 0, true)
	if !strings.Contains(out.String(), "▾ done (1)") || !strings.Contains(out.String(), "d1") {
		t.Errorf("--all-done:\n%s", out.String())
	}
}

func TestListGroupFlagsAndTheJSONStayHonest(t *testing.T) {
	rigEnv(t)
	d := &inProcessDaemon{}
	t.Cleanup(func() { d.shutdown(t) })
	deps := deps{spawner: d.spawn}
	if _, _, code := rig(t, deps, "list", "--all-done"); code != 2 {
		t.Errorf("--all-done without --group exited %d, want 2", code)
	}
	if _, _, code := rig(t, deps, "list", "--group", "--all"); code != 2 {
		t.Errorf("--group with --all exited %d, want 2", code)
	}
	stdout, _, code := rig(t, deps, "list", "--group")
	if code != 0 || !strings.Contains(stdout, "no tickets") {
		t.Errorf("--group with none: %d %q", code, stdout)
	}
}

// colAt is where the column named name starts in the table's header line.
func colAt(t *testing.T, out, name string) int {
	t.Helper()
	i := strings.Index(strings.SplitN(out, "\n", 2)[0], name)
	if i >= 0 {
		return i // the header line is ASCII
	}
	if i < 0 {
		t.Fatalf("no column %s:\n%s", name, out)
	}
	return i
}

// lineWith is the first line starting with prefix.
func lineWith(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starting %q:\n%s", prefix, out)
	return ""
}

func TestGroupHeadersAndFooterSitUnderTheirColumns(t *testing.T) {
	now := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	for _, width := range []int{0, 120, 70} {
		var out strings.Builder
		writeGroupedTable(&out, groupedFixture(), now, width, false)
		o := out.String()
		cost, tm, agent := colAt(t, o, "COST"), colAt(t, o, "TIME"), colAt(t, o, "AGENT")
		for prefix, want := range map[string][3]string{
			"▾ implement (2)": {"$4.12", "2h00m", "1h20m"},
			"▸ done (1)":      {"$1.50", "1h00m", "10m"},
			"all (4)":         {"$5.62", "2h00m", "1h30m"},
		} {
			l := []rune(lineWith(t, o, prefix))
			for i, c := range [3]int{cost, tm, agent} {
				if !strings.HasPrefix(string(l[c:]), want[i]) {
					t.Errorf("width %d: %q has no %q at column %d:\n%s", width, prefix, want[i], c, o)
				}
			}
		}
		if strings.Contains(o, " · ") {
			t.Errorf("the joined header text is gone:\n%s", o)
		}
	}
}

func TestAGroupHeaderShowsWhatIsTheSameForEveryTicketAndWidensID(t *testing.T) {
	now := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	svc := []api.ServiceSummary{{Name: "rig", Status: "stopped", AgentState: domain.AgentExited}}
	var tickets []api.TicketSummary
	for i := 0; i < 34; i++ {
		tickets = append(tickets, api.TicketSummary{ID: fmt.Sprintf("r%d", i), Phase: domain.PhaseDone, Services: svc})
	}
	tickets = append(tickets, api.TicketSummary{ID: "i1", Phase: domain.PhaseImplement, Services: []api.ServiceSummary{{Name: "rig", Status: "running"}}})
	var out strings.Builder
	writeGroupedTable(&out, tickets, now, 0, false)
	o := out.String()
	services := colAt(t, o, "SERVICES")
	if services < len("▸ done (34)")+2 {
		t.Errorf("the ID column must widen to the label:\n%s", o)
	}
	if l := []rune(lineWith(t, o, "▸ done (34)")); !strings.HasPrefix(string(l[services:]), "rig:stopped/exited") {
		t.Errorf("the common status shows:\n%s", o)
	}
	if l := lineWith(t, o, "all (35)"); strings.Contains(l, "rig:") {
		t.Errorf("a mixed footer leaves SERVICES blank:\n%s", o)
	}
}
