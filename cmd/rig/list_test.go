package main

import (
	"context"
	"encoding/json"
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
	if code != 0 || !strings.Contains(listing, "(contract change pending)") || !strings.Contains(listing, "(contract edited outside rig)") {
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
