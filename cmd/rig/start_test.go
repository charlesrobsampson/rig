package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func newTicket(t *testing.T, d deps, id string, repos ...*testkit.Repo) {
	t.Helper()
	args := []string{"new", "--id", id, "--title", "Ticket " + id}
	for i, r := range repos {
		args = append(args, "--service", "svc"+string(rune('1'+i))+"="+r.Dir)
	}
	if _, stderr, code := rig(t, d, args...); code != 0 {
		t.Fatalf("rig new failed: %s", stderr)
	}
}

func TestStartProvisionsCheckoutsThatListShows(t *testing.T) {
	home, d, repo := newEnv(t)
	other := testkit.NewRepo(t)
	newTicket(t, d, "demo", repo, other)

	stdout, stderr, code := rig(t, d, "start", "demo")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"svc1", "svc2", "clone", filepath.Join(home, "tickets", "demo", "svc1"), "created"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	co := filepath.Join(home, "tickets", "demo", "svc1")
	if branch := testkit.RunGit(t, co, "rev-parse", "--abbrev-ref", "HEAD"); branch != "rig/demo/svc1" {
		t.Errorf("checkout is on %q", branch)
	}

	listing, _, _ := rig(t, d, "list")
	if !strings.Contains(listing, "svc1:provisioned") || !strings.Contains(listing, "svc2:provisioned") {
		t.Errorf("list after start:\n%s", listing)
	}

	again, _, code := rig(t, d, "start", "demo")
	if code != 0 || strings.Contains(again, "created") || !strings.Contains(again, "existing") {
		t.Errorf("a second start: code %d, stdout:\n%s", code, again)
	}
}

func TestStartSandboxFlag(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	stdout, stderr, code := rig(t, d, "start", "demo", "--sandbox", "host")
	if code != 0 || !strings.Contains(stdout, "worktree") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if info, err := os.Stat(filepath.Join(home, "tickets", "demo", "svc1", ".git")); err != nil || info.IsDir() {
		t.Errorf("a host-sandbox checkout should be a worktree with a .git pointer file: %v", err)
	}
}

func TestStartJSON(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	stdout, _, code := rig(t, d, "start", "demo", "--json")
	var got api.TicketStartResult
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || len(got.Checkouts) != 1 || got.Checkouts[0].Kind != domain.CheckoutClone || !got.Checkouts[0].Created {
		t.Errorf("code %d, %q, %v", code, stdout, err)
	}
}

func TestStartErrors(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	tests := []struct {
		name string
		args []string
		code int
		text string
	}{
		{"no ticket id", []string{"start"}, 2, "arg"},
		{"too many arguments", []string{"start", "a", "b"}, 2, "arg"},
		{"an invalid sandbox", []string{"start", "demo", "--sandbox", "vm"}, 2, "sandbox"},
		{"an unknown ticket", []string{"start", "nope"}, 5, "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := rig(t, d, tt.args...)
			if code != tt.code || !strings.Contains(stderr, tt.text) || stdout != "" {
				t.Errorf("code %d (want %d), stdout %q, stderr %q", code, tt.code, stdout, stderr)
			}
		})
	}
}

func TestStartDryRunShowsTheLaunchAndChangesNothing(t *testing.T) {
	home, d, repo := newEnv(t)
	other := testkit.NewRepo(t)
	newTicket(t, d, "demo", repo, other)

	stdout, stderr, code := rig(t, d, "start", "demo", "--dry-run")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"service svc1 (claude-code)", "service svc2 (claude-code)",
		"dir: " + filepath.Join(home, "tickets", "demo", "svc1"),
		"    claude", "    --settings", "RIG_TICKET=demo", "RIG_SERVICE=svc2", "bytes",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, `"hooks"`) {
		t.Errorf("long arguments should be summarized by size:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(home, "tickets", "demo", "svc1")); !os.IsNotExist(err) {
		t.Errorf("a dry run created a checkout: %v", err)
	}
	if listing, _, _ := rig(t, d, "list"); strings.Contains(listing, "provisioned") {
		t.Errorf("a dry run changed the ticket:\n%s", listing)
	}
}

func TestStartDryRunJSONHasTheFullCommand(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	stdout, _, code := rig(t, d, "start", "demo", "--dry-run", "--json")
	var got api.TicketStartResult
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || len(got.Launches) != 1 || len(got.Checkouts) != 0 {
		t.Fatalf("code %d, %q, %v", code, stdout, err)
	}
	cmd := got.Launches[0].Command
	if len(cmd) != 6 || cmd[0] != "claude" || !strings.Contains(cmd[2], "agent-event") || cmd[3] != "--add-dir" || !strings.Contains(cmd[5], `service "svc1"`) {
		t.Errorf("command = %q", cmd)
	}
}

func TestStartShowsTheTicketNetwork(t *testing.T) {
	_, d, repo, _, dk := newEnvFull(t)
	newTicket(t, d, "demo", repo)
	stdout, stderr, code := rig(t, d, "start", "demo")
	if code != 0 || !strings.Contains(stdout, "network rig-demo (bridge)") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if n, err := dk.InspectNetwork(context.Background(), "rig-demo"); err != nil || n.Labels["rig.ticket"] != "demo" {
		t.Errorf("network = %+v, %v", n, err)
	}
	json, _, _ := rig(t, d, "start", "demo", "--json")
	if !strings.Contains(json, `"network"`) || !strings.Contains(json, `"rig-demo"`) {
		t.Errorf("--json lacks the network: %s", json)
	}
	if _, stderr, code := rig(t, d, "stop", "demo"); code != 0 {
		t.Fatalf("stop: %q", stderr)
	}
	if list, _ := dk.ListNetworks(context.Background(), nil); len(list) != 0 {
		t.Errorf("networks after stop: %v", list)
	}
}

func TestStartRunsServiceContainersAndShowsTheirURLs(t *testing.T) {
	home, d, repo, _, dk := newEnvFull(t)
	repo.Commit("Dockerfile", "FROM scratch\n", "add a Dockerfile")
	newTicket(t, d, "demo", repo)
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	os.WriteFile(yamlPath, []byte(strings.Replace(string(raw), "      branch: rig/", "      build: {dockerfile: Dockerfile}\n      internal_port: 8080\n      branch: rig/", 1)), 0o600)

	stdout, stderr, code := rig(t, d, "start", "demo")
	if code != 0 || !strings.Contains(stdout, "service svc1: running at http://localhost:41") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	listing, _, _ := rig(t, d, "list")
	if !strings.Contains(listing, "svc1:running") || !strings.Contains(listing, "41") {
		t.Errorf("list should show the running service and its port:\n%s", listing)
	}
	list, _ := dk.ListContainers(context.Background(), map[string]string{"rig.ticket": "demo"})
	if len(list) != 1 || list[0].Name != "rig-demo-svc1" {
		t.Errorf("containers = %+v", list)
	}

	dk.Down(nil)
	rig(t, d, "stop", "demo")
	if left, _ := dk.ListContainers(context.Background(), nil); len(left) != 0 {
		t.Errorf("containers after stop: %v", left)
	}
}

func TestStartFailsLoudlyWhenAServiceBuildFails(t *testing.T) {
	home, d, repo, _, _ := newEnvFull(t)
	newTicket(t, d, "demo", repo) // no Dockerfile in the repo
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	os.WriteFile(yamlPath, []byte(strings.Replace(string(raw), "      branch: rig/", "      build: {dockerfile: Dockerfile}\n      branch: rig/", 1)), 0o600)
	_, stderr, code := rig(t, d, "start", "demo")
	if code != 2 || !strings.Contains(stderr, "no Dockerfile") {
		t.Errorf("code %d (want 2, invalid), stderr %q", code, stderr)
	}
}
