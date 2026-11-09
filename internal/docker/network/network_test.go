package network_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/network"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var (
	ctx   = context.Background()
	epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

func setup() (*testkit.FakeDocker, *testkit.FakeClock, *network.Manager) {
	d := testkit.NewFakeDocker()
	d.AddImage("img")
	clock := testkit.NewFakeClock(epoch)
	return d, clock, network.New(d, clock)
}

func names(t *testing.T, d docker.Docker) []string {
	t.Helper()
	list, err := d.ListNetworks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range list {
		out = append(out, n.Name)
	}
	return out
}

func TestEnsureCreatesTheNetworkForTheMode(t *testing.T) {
	tests := []struct {
		name         string
		mode         domain.NetworkMode
		wantInternal bool
		wantEgress   bool
	}{
		{"bridge", domain.NetworkBridge, false, false},
		{"internal", domain.NetworkInternal, true, true},
		{"no mode means bridge", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _, m := setup()
			nets, err := m.Ensure(ctx, "demo", tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			if nets.Main.Name != "rig-demo" || nets.Main.Internal != tt.wantInternal {
				t.Errorf("main = %+v", nets.Main)
			}
			if (nets.Egress != nil) != tt.wantEgress {
				t.Fatalf("egress = %+v, want present=%v", nets.Egress, tt.wantEgress)
			}
			if tt.wantEgress {
				if nets.Egress.Name != "rig-demo-egress" || nets.Egress.Internal {
					t.Errorf("egress = %+v, want a normal bridge named rig-demo-egress", nets.Egress)
				}
			}
			for _, info := range []docker.NetworkInfo{nets.Main} {
				if info.Labels[docker.LabelTicket] != "demo" || info.Labels[docker.LabelRole] != docker.RoleNetwork {
					t.Errorf("labels = %v", info.Labels)
				}
			}
			if got := len(names(t, d)); got != map[bool]int{false: 1, true: 2}[tt.wantEgress] {
				t.Errorf("%d networks exist", got)
			}
		})
	}
}

func TestEnsureIsRepeatable(t *testing.T) {
	d, _, m := setup()
	first, err := m.Ensure(ctx, "demo", domain.NetworkInternal)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure(ctx, "demo", domain.NetworkInternal)
	if err != nil || second.Main.ID != first.Main.ID || second.Egress.ID != first.Egress.ID {
		t.Errorf("second Ensure = %+v, %v; want the same networks", second, err)
	}
	if got := names(t, d); len(got) != 2 {
		t.Errorf("networks = %v, want exactly two", got)
	}
}

func TestTwoTicketsHaveSeparateNetworks(t *testing.T) {
	d, _, m := setup()
	a, _ := m.Ensure(ctx, "one", domain.NetworkBridge)
	b, _ := m.Ensure(ctx, "two", domain.NetworkBridge)
	if a.Main.ID == b.Main.ID || a.Main.Name == b.Main.Name {
		t.Errorf("one = %+v, two = %+v", a.Main, b.Main)
	}
	if err := m.Remove(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, d); len(got) != 1 || got[0] != "rig-two" {
		t.Errorf("networks after removing one's = %v", got)
	}
}

func TestEnsureRefusesANetworkWithTheWrongIsolation(t *testing.T) {
	d, _, m := setup()
	if _, err := m.Ensure(ctx, "demo", domain.NetworkBridge); err != nil {
		t.Fatal(err)
	}
	_, err := m.Ensure(ctx, "demo", domain.NetworkInternal)
	if !errors.Is(err, network.ErrConflict) || !strings.Contains(err.Error(), "internal=false") {
		t.Errorf("error = %v, want a conflict about the internal flag", err)
	}
	if got := names(t, d); len(got) != 1 {
		t.Errorf("a refused Ensure created %v", got)
	}
}

func TestRigNeverUsesOrRemovesANetworkItDidNotMake(t *testing.T) {
	d, _, m := setup()
	if _, err := d.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-demo"}); err != nil { // someone else's, no labels
		t.Fatal(err)
	}
	if _, err := m.Ensure(ctx, "demo", domain.NetworkBridge); !errors.Is(err, network.ErrConflict) {
		t.Errorf("Ensure = %v, want ErrConflict", err)
	}
	if err := m.Remove(ctx, "demo"); !errors.Is(err, network.ErrConflict) {
		t.Errorf("Remove = %v, want ErrConflict", err)
	}
	if got := names(t, d); len(got) != 1 {
		t.Errorf("the foreign network was touched: %v", got)
	}
}

func TestRemoveDeletesBothNetworksAndIsRepeatable(t *testing.T) {
	d, _, m := setup()
	m.Ensure(ctx, "demo", domain.NetworkInternal)
	if err := m.Remove(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, d); len(got) != 0 {
		t.Errorf("networks left: %v", got)
	}
	if err := m.Remove(ctx, "demo"); err != nil {
		t.Errorf("removing what is already gone: %v", err)
	}
}

func attach(t *testing.T, d *testkit.FakeDocker, name, net string) {
	t.Helper()
	c, err := d.CreateContainer(ctx, docker.ContainerSpec{Name: name, Image: "img", Network: net})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartContainer(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveWaitsForContainersToDetach(t *testing.T) {
	d, clock, m := setup()
	m.Ensure(ctx, "demo", domain.NetworkBridge)
	attach(t, d, "rig-demo-api", "rig-demo")

	done := make(chan error, 1)
	go func() { done <- m.Remove(ctx, "demo") }()
	for range 8 { // two seconds pass with the container still attached
		clock.Advance(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("Remove returned early: %v", err)
	default:
	}
	if err := d.RemoveContainer(ctx, "rig-demo-api", true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if got := names(t, d); len(got) != 0 {
				t.Errorf("networks left: %v", got)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Remove never finished after the container left")
		}
		clock.Advance(250 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

func TestRemoveGivesUpAndNamesWhatIsStillAttached(t *testing.T) {
	d, clock, m := setup()
	m.Ensure(ctx, "demo", domain.NetworkBridge)
	attach(t, d, "rig-demo-api", "rig-demo")
	attach(t, d, "rig-demo-dep-db", "rig-demo")

	done := make(chan error, 1)
	go func() { done <- m.Remove(ctx, "demo") }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			if !errors.Is(err, docker.ErrInUse) || !strings.Contains(err.Error(), "rig-demo-api") || !strings.Contains(err.Error(), "rig-demo-dep-db") {
				t.Errorf("error = %v, want ErrInUse naming both containers", err)
			}
			if got := names(t, d); len(got) != 1 {
				t.Errorf("the network should remain: %v", got)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Remove never gave up")
		}
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

func TestDockerDownIsUnavailable(t *testing.T) {
	d, _, m := setup()
	d.Down(errors.New("daemon stopped"))
	if _, err := m.Ensure(ctx, "demo", domain.NetworkBridge); !errors.Is(err, docker.ErrUnavailable) {
		t.Errorf("Ensure = %v", err)
	}
	if err := m.Remove(ctx, "demo"); !errors.Is(err, docker.ErrUnavailable) {
		t.Errorf("Remove = %v", err)
	}
}
