package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
)

func TestReconcileOnACleanSystemSaysSo(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	stdout, stderr, code := rig(t, d, "reconcile")
	if code != 0 || !strings.Contains(stdout, "no drift") {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if out, _, _ := rig(t, d, "list"); strings.Contains(out, "drift") {
		t.Errorf("list shows drift on a clean system:\n%s", out)
	}
}

func TestAKilledContainerShowsAsDriftInReconcileAndList(t *testing.T) {
	_, d, repo, _, dk := newEnvFull(t)
	newTicket(t, d, "demo", repo)
	dk.AddImage("img")
	// a ticket the daemon believes has a running container that is gone
	statePath := filepath.Join(os.Getenv("RIG_HOME"), "tickets", "demo", "state.json")
	raw, _ := os.ReadFile(statePath)
	var st map[string]any
	json.Unmarshal(raw, &st)
	st["services"] = map[string]any{"svc1": map[string]any{"status": "running", "container_id": "ctr-vanished", "checkout": map[string]any{"path": ""}}}
	raw, _ = json.MarshalIndent(st, "", "  ")
	os.WriteFile(statePath, raw, 0o600)

	stdout, _, code := rig(t, d, "reconcile")
	if code != 0 || !strings.Contains(stdout, "service_container_missing") || !strings.Contains(stdout, "svc1") || !strings.Contains(stdout, "is gone") {
		t.Fatalf("code %d, stdout:\n%s", code, stdout)
	}
	out, _, _ := rig(t, d, "list")
	if !strings.Contains(out, "(drift: 1, see rig reconcile)") {
		t.Errorf("list:\n%s", out)
	}
	jsonOut, _, _ := rig(t, d, "list", "--json")
	var res api.TicketListResult
	json.Unmarshal([]byte(jsonOut), &res)
	if len(res.Tickets[0].Drift) != 1 || !strings.Contains(res.Tickets[0].Drift[0], "svc1: the service's container is gone") {
		t.Errorf("--json drift = %v", res.Tickets[0].Drift)
	}
}

func TestReconcileFixRemovesOnlyRigOrphansAfterConfirmation(t *testing.T) {
	_, d, repo, _, dk := newEnvFull(t)
	newTicket(t, d, "demo", repo)
	dk.AddImage("img")
	ctx := context.Background()
	mk := func(name string, labels map[string]string) docker.ContainerInfo {
		c, err := dk.CreateContainer(ctx, docker.ContainerSpec{Name: name, Image: "img", Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		dk.StartContainer(ctx, c.ID)
		return c
	}
	orphan := mk("rig-ghost-api", docker.Labels("ghost", "api", docker.RoleService))
	foreign := mk("not-rigs", map[string]string{"app": "nginx"})

	stdout, _, _ := rig(t, d, "reconcile")
	if !strings.Contains(stdout, "orphan_container") || !strings.Contains(stdout, "rig reconcile --fix") {
		t.Fatalf("report:\n%s", stdout)
	}
	// saying no removes nothing
	d.stdin = strings.NewReader("n\n")
	_, stderr, code := rig(t, d, "reconcile", "--fix")
	if code == 0 || !strings.Contains(stderr, "rig-ghost-api") || !strings.Contains(stderr, "Remove 1 orphan(s)? [y/N]") {
		t.Errorf("declined: code %d, stderr %q", code, stderr)
	}
	if _, err := dk.InspectContainer(ctx, orphan.ID); err != nil {
		t.Fatal("declining removed the orphan")
	}
	// yes removes the orphan and nothing else
	d.stdin = strings.NewReader("y\n")
	stdout, stderr, code = rig(t, d, "reconcile", "--fix")
	if code != 0 || !strings.Contains(stdout, "removed: container rig-ghost-api") || !strings.Contains(stdout, "no drift") {
		t.Fatalf("fix: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := dk.InspectContainer(ctx, orphan.ID); err == nil {
		t.Error("the orphan survived")
	}
	if got, err := dk.InspectContainer(ctx, foreign.ID); err != nil || got.State != docker.StateRunning {
		t.Errorf("a container that is not rig's was touched: %v", err)
	}
	// --yes skips the question; nothing to remove now
	d.stdin = nil
	if out, _, code := rig(t, d, "reconcile", "--fix", "--yes"); code != 0 || !strings.Contains(out, "no drift") {
		t.Errorf("nothing to fix: %d %q", code, out)
	}
}

func TestAgentsAndStateSurviveADaemonRestart(t *testing.T) {
	home, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", "working")
	panes, _ := term.List(context.Background())
	if len(panes) != 1 {
		t.Fatalf("panes = %v", panes)
	}
	before, _, _ := rig(t, d, "list", "--json")
	stateBefore, _ := os.ReadFile(filepath.Join(home, "tickets", "demo", "state.json"))

	// kill the daemon (the fakes, like the real panes and containers, outlive it) and start another
	if _, _, code := rig(t, d, "daemon", "stop"); code != 0 {
		t.Fatalf("daemon stop failed")
	}
	time.Sleep(100 * time.Millisecond)
	stdout, stderr, code := rig(t, d, "reconcile")
	if code != 0 || !strings.Contains(stdout, "no drift") {
		t.Fatalf("after the restart: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	after, _, _ := rig(t, d, "list", "--json")
	if before != after {
		t.Errorf("the fleet changed across the restart:\nbefore %s\nafter  %s", before, after)
	}
	stateAfter, _ := os.ReadFile(filepath.Join(home, "tickets", "demo", "state.json"))
	var a, b map[string]any
	json.Unmarshal(stateBefore, &a)
	json.Unmarshal(stateAfter, &b)
	delete(a, "updated_at")
	delete(b, "updated_at")
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("state.json changed across the restart:\n%s\n%s", ja, jb)
	}
	if list, _ := term.List(context.Background()); len(list) != 1 || !list[0].Alive {
		t.Errorf("the agent's pane did not survive: %v", list)
	}
}
