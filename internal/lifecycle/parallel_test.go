package lifecycle_test

import (
	"path/filepath"
	"testing"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// Two tickets of one repository, each with the same service and the same sidecar names,
// must run side by side without sharing or colliding on anything.
func TestTwoTicketsOfTheSameServiceRunSideBySide(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t) // one repository behind both tickets
	repo.Commit("Dockerfile", "FROM scratch\n", "add a Dockerfile")
	for _, id := range []string{"alpha", "beta"} {
		path := f.create(id, repo)
		withService(t, path, repo, false, apiService)
		addDeps(t, path, dbDeps)
	}

	a := f.mustStart("alpha", domain.SandboxContainer)
	b := f.mustStart("beta", domain.SandboxContainer)

	// Everything is distinct.
	if a.Network.Name == b.Network.Name || a.Network.ID == b.Network.ID {
		t.Errorf("networks: %+v and %+v", a.Network, b.Network)
	}
	ca, cb := containerByService(a)["svc1"], containerByService(b)["svc1"]
	if !ca.Running || !cb.Running || ca.HostPort == cb.HostPort || ca.ID == cb.ID || ca.Name == cb.Name {
		t.Errorf("services: %+v and %+v", ca, cb)
	}
	if a.Dependencies[0].ID == b.Dependencies[0].ID || a.Dependencies[0].Name == b.Dependencies[0].Name {
		t.Errorf("dependencies: %+v and %+v", a.Dependencies, b.Dependencies)
	}
	if a.Checkouts[0].Path == b.Checkouts[0].Path || a.Checkouts[0].Branch == b.Checkouts[0].Branch {
		t.Errorf("checkouts: %+v and %+v", a.Checkouts, b.Checkouts)
	}
	for _, c := range []string{"alpha", "beta"} {
		if list, _ := dk.ListContainers(ctx, map[string]string{docker.LabelTicket: c}); len(list) != 2 {
			t.Errorf("ticket %s has %d containers, want a service and a sidecar", c, len(list))
		}
	}

	// Each service reaches its own sidecar under the same name.
	ipA, errA := dk.Resolve("rig-alpha-svc1", "db")
	ipB, errB := dk.Resolve("rig-beta-svc1", "db")
	if errA != nil || errB != nil || ipA == ipB {
		t.Errorf("db resolves to %q (%v) from alpha and %q (%v) from beta", ipA, errA, ipB, errB)
	}
	if ip, _ := dk.Resolve("rig-alpha-svc1", "db"); ip != resolveName(t, dk, "rig-alpha-dep-db") {
		t.Errorf("alpha's db is not alpha's sidecar")
	}
	if _, err := dk.Resolve("rig-alpha-svc1", "rig-beta-dep-db"); err == nil {
		t.Error("alpha can reach beta's sidecar by its container name")
	}

	// Stopping one leaves the other untouched.
	betaState := f.state("beta")
	f.mustStop("alpha", false, false)
	if list, _ := dk.ListContainers(ctx, map[string]string{docker.LabelTicket: "alpha"}); len(list) != 0 {
		t.Errorf("alpha's containers remain: %v", list)
	}
	if _, err := dk.InspectNetwork(ctx, "rig-alpha"); err == nil {
		t.Error("alpha's network remains")
	}
	for _, name := range []string{"rig-beta-svc1", "rig-beta-dep-db"} {
		if c, err := dk.InspectContainer(ctx, name); err != nil || c.State != docker.StateRunning {
			t.Errorf("%s = %+v, %v; stopping alpha must not touch it", name, c, err)
		}
	}
	if _, err := dk.InspectNetwork(ctx, "rig-beta"); err != nil {
		t.Errorf("beta's network is gone: %v", err)
	}
	if _, err := dk.Resolve("rig-beta-svc1", "db"); err != nil {
		t.Errorf("beta lost its sidecar: %v", err)
	}
	after := f.state("beta")
	if after.Services["svc1"].HostPort != betaState.Services["svc1"].HostPort || after.Services["svc1"].ContainerID != betaState.Services["svc1"].ContainerID || after.Network != betaState.Network {
		t.Errorf("beta's state changed: %+v -> %+v", betaState.Services["svc1"], after.Services["svc1"])
	}
	if ports := dk.HostPorts(); len(ports) != 1 || ports[0] != cb.HostPort {
		t.Errorf("host ports held = %v, want only beta's %d", ports, cb.HostPort)
	}
	if !exists(filepath.Join(f.dir, "beta", "svc1")) {
		t.Error("beta's checkout is gone")
	}

	// alpha's port is free for a new ticket again.
	path := f.create("gamma", repo)
	withService(t, path, repo, false, apiService)
	g := containerByService(f.mustStart("gamma", domain.SandboxContainer))["svc1"]
	if g.HostPort != ca.HostPort {
		t.Errorf("gamma got port %d, want alpha's freed %d (the lowest free)", g.HostPort, ca.HostPort)
	}
}

func resolveName(t *testing.T, dk *testkit.FakeDocker, container string) string {
	t.Helper()
	ip, err := dk.Resolve(container, container)
	if err != nil {
		t.Fatal(err)
	}
	return ip
}
