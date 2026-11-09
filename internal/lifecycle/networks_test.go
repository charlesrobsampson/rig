package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func dockerFixture(t *testing.T, mode domain.NetworkMode) (*fixture, *testkit.FakeDocker, *testkit.FakeClock) {
	t.Helper()
	f := newFixture(t, domain.SandboxContainer)
	dk := testkit.NewFakeDocker()
	dk.AddImage("img")
	clock := testkit.NewFakeClock(epoch)
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: clock, Docker: dk,
		EgressBinary: fakeProxyBinary, Settle: -1, Global: config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, NetworkMode: mode, BranchPrefix: "rig/"}},
	})
	return f, dk, clock
}

func networkNames(t *testing.T, dk *testkit.FakeDocker) []string {
	t.Helper()
	list, _ := dk.ListNetworks(ctx, nil)
	var out []string
	for _, n := range list {
		out = append(out, n.Name)
	}
	return out
}

func TestStartCreatesTheTicketNetworkOnce(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkBridge)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	if res.Network == nil || res.Network.Name != "rig-demo" || res.Network.Internal || res.Network.Egress != "" {
		t.Fatalf("network = %+v", res.Network)
	}
	again := f.mustStart("demo", domain.SandboxContainer)
	if again.Network.ID != res.Network.ID || len(networkNames(t, dk)) != 1 {
		t.Errorf("a second start made another network: %v", networkNames(t, dk))
	}
	st := f.state("demo")
	if st.Network.Name != "rig-demo" || st.Network.ID != res.Network.ID || st.EgressNetwork != nil {
		t.Errorf("state network = %+v, egress %+v", st.Network, st.EgressNetwork)
	}
}

func TestInternalModeCreatesAnInternalNetworkAndTheEgressBridge(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkInternal)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	if res.Network == nil || !res.Network.Internal || res.Network.Egress != "rig-demo-egress" {
		t.Fatalf("network = %+v", res.Network)
	}
	main, _ := dk.InspectNetwork(ctx, "rig-demo")
	egress, _ := dk.InspectNetwork(ctx, "rig-demo-egress")
	if !main.Internal || egress.Internal {
		t.Errorf("internal flags: main %v, egress %v", main.Internal, egress.Internal)
	}
	if st := f.state("demo"); st.EgressNetwork == nil || st.EgressNetwork.Name != "rig-demo-egress" {
		t.Errorf("egress not recorded: %+v", st.EgressNetwork)
	}
}

func TestTheTicketsNetworkModeComesFromTicketYaml(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkBridge)
	path := f.create("demo", testkit.NewRepo(t))
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("docker:\n  network_mode: internal\n")...), 0o600)
	f.mustStart("demo", domain.SandboxContainer)
	if n, err := dk.InspectNetwork(ctx, "rig-demo"); err != nil || !n.Internal {
		t.Errorf("network = %+v, %v; the ticket asked for internal", n, err)
	}
}

func TestTwoTicketsGetTheirOwnNetworks(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkBridge)
	f.create("one", testkit.NewRepo(t))
	f.create("two", testkit.NewRepo(t))
	f.mustStart("one", domain.SandboxContainer)
	f.mustStart("two", domain.SandboxContainer)
	if got := networkNames(t, dk); strings.Join(got, ",") != "rig-one,rig-two" {
		t.Errorf("networks = %v", got)
	}
}

func TestAHostOnlyTicketNeedsNoDocker(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkBridge)
	f.create("demo", testkit.NewRepo(t))
	dk.Down(docker.ErrUnavailable) // Docker is not even running
	res, err := f.start("demo", domain.SandboxHost)
	if err != nil || res.Network != nil {
		t.Errorf("start = %+v, %v; a host-only ticket must not need Docker", res.Network, err)
	}
}

func TestStartNeedsDockerForContainerWork(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkBridge)
	f.create("demo", testkit.NewRepo(t))
	dk.Down(docker.ErrUnavailable)
	_, err := f.start("demo", domain.SandboxContainer)
	if apiCode(t, err) != api.CodeUnavailable || !strings.Contains(err.Error(), "network") {
		t.Errorf("error = %v, want unavailable mentioning the network", err)
	}
	if !exists(filepath.Join(f.dir, "demo", "svc1")) {
		t.Error("the checkout should stay; starting again retries")
	}
	dk.Up()
	if res := f.mustStart("demo", domain.SandboxContainer); res.Network == nil {
		t.Error("the retry did not create the network")
	}
}

func TestAMismatchedExistingNetworkIsAConflict(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkInternal)
	f.create("demo", testkit.NewRepo(t))
	dk.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-demo", Labels: docker.Labels("demo", "", docker.RoleNetwork)}) // not internal
	_, err := f.start("demo", domain.SandboxContainer)
	if apiCode(t, err) != api.CodeConflict || !strings.Contains(err.Error(), "internal") {
		t.Errorf("error = %v", err)
	}
}

func TestStopRemovesTheNetworkAndForgetsIt(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkInternal)
	f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxContainer)
	f.mustStop("demo", false, false)
	if got := networkNames(t, dk); len(got) != 0 {
		t.Errorf("networks left: %v", got)
	}
	if st := f.state("demo"); st.Network.Name != "" || st.EgressNetwork != nil {
		t.Errorf("state still names the network: %+v %+v", st.Network, st.EgressNetwork)
	}
}

func TestStopWithAContainerStillAttachedKeepsEverything(t *testing.T) {
	f, dk, clock := dockerFixture(t, domain.NetworkBridge)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	c, _ := dk.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-demo-api", Image: "img", Network: "rig-demo"})
	dk.StartContainer(ctx, c.ID)

	done := make(chan error, 1)
	go func() { _, err := f.stop("demo", false, false); done <- err }()
	deadline := time.After(30 * time.Second)
	for {
		clock.Advance(time.Second) // the wait for the container to detach runs on the fake clock
		select {
		case err := <-done:
			if apiCode(t, err) != api.CodeConflict || !strings.Contains(err.Error(), "rig-demo-api") {
				t.Errorf("error = %v, want a conflict naming the container", err)
			}
			if !exists(res.Checkouts[0].Path) || len(networkNames(t, dk)) != 1 {
				t.Error("a refused stop removed something")
			}
			return
		case <-deadline:
			t.Fatal("stop never gave up")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStopOfARefusedDirtyCheckoutLeavesTheNetwork(t *testing.T) {
	f, dk, _ := dockerFixture(t, domain.NetworkBridge)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	os.WriteFile(filepath.Join(res.Checkouts[0].Path, "uncommitted.txt"), []byte("x"), 0o600)
	if _, err := f.stop("demo", false, false); apiCode(t, err) != api.CodeBlocked {
		t.Fatalf("error = %v", err)
	}
	if got := networkNames(t, dk); len(got) != 1 {
		t.Errorf("networks = %v; a refused stop must not tear the network down", got)
	}
}

// fakeProxyBinary stands in for the embedded egress proxy, which default tests must not need.
func fakeProxyBinary(string) ([]byte, error) { return []byte("proxy"), nil }
