package testkit_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/dockertest"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestFakeDockerSatisfiesTheContract(t *testing.T) {
	dockertest.Run(t, func(t *testing.T) dockertest.Subject {
		fake := testkit.NewFakeDocker()
		fake.AddImage("busybox")
		next := 45000
		return dockertest.Subject{
			Docker: fake,
			Image:  "busybox",
			FreePort: func(*testing.T) int {
				next++
				return next
			},
			Exiting: func(t *testing.T, spec docker.ContainerSpec, code int) string {
				c, err := fake.CreateContainer(context.Background(), spec)
				if err != nil {
					t.Fatal(err)
				}
				if err := fake.StartContainer(context.Background(), c.ID); err != nil {
					t.Fatal(err)
				}
				fake.Exit(c.ID, code)
				return c.ID
			},
			Print:       func(t *testing.T, id, line string) { fake.Print(id, line) },
			MakeHealthy: func(t *testing.T, id string) { fake.SetHealth(id, docker.HealthHealthy) },
			Down:        func(t *testing.T) { fake.Down(errors.New("cannot connect to the Docker daemon")) },
		}
	})
}

func TestFakeDockerNameResolutionFollowsNetworksAndAliases(t *testing.T) {
	ctx := context.Background()
	fake := testkit.NewFakeDocker()
	fake.AddImage("img")
	mk := func(name, network string, aliases ...string) string {
		c, err := fake.CreateContainer(ctx, docker.ContainerSpec{Name: name, Image: "img", Network: network, Aliases: aliases})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	for _, n := range []string{"rig-a", "rig-b"} {
		if _, err := fake.CreateNetwork(ctx, docker.NetworkSpec{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	apiA, dbA := mk("api-a", "rig-a"), mk("rig-a-dep-db", "rig-a", "db")
	apiB, dbB := mk("api-b", "rig-b"), mk("rig-b-dep-db", "rig-b", "db")
	alone := mk("alone", "")

	if _, err := fake.Resolve(apiA, "db"); err == nil {
		t.Error("a container that is not running resolved")
	}
	for _, id := range []string{apiA, dbA, apiB, dbB, alone} {
		if err := fake.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	ipA, errA := fake.Resolve(apiA, "db")
	ipB, errB := fake.Resolve(apiB, "db")
	if errA != nil || errB != nil || ipA == ipB {
		t.Errorf("db from ticket a = %q (%v), from ticket b = %q (%v); each must reach its own sidecar", ipA, errA, ipB, errB)
	}
	if ip, err := fake.Resolve(apiA, "rig-a-dep-db"); err != nil || ip != ipA {
		t.Errorf("a container resolves by its own name: %q, %v", ip, err)
	}
	for _, name := range []string{"api-b", "rig-b-dep-db", "nope"} {
		if _, err := fake.Resolve(apiA, name); !errors.Is(err, docker.ErrNotFound) {
			t.Errorf("%s from the other network = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := fake.Resolve(alone, "db"); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("a container on no network resolved db: %v", err)
	}
	fake.Exit(dbA, 1)
	if _, err := fake.Resolve(apiA, "db"); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("an exited container still resolves: %v", err)
	}
}

func TestFakeDockerPortsAndScripting(t *testing.T) {
	ctx := context.Background()
	fake := testkit.NewFakeDocker()
	fake.AddImage("img")
	start := func(name string, host int) (docker.ContainerInfo, error) {
		c, err := fake.CreateContainer(ctx, docker.ContainerSpec{Name: name, Image: "img", Ports: []docker.PortBinding{{Container: 80, Host: host}}})
		if err != nil {
			t.Fatal(err)
		}
		return c, fake.StartContainer(ctx, name)
	}
	start("a", 0)
	start("b", 0)
	start("c", 41000)
	ports := fake.HostPorts()
	if len(ports) != 3 || ports[0] == ports[1] || ports[2] != 41000 && ports[1] != 41000 {
		t.Errorf("host ports = %v, want three distinct ports including 41000", ports)
	}
	fake.Exit("a", 0)
	if got := fake.HostPorts(); len(got) != 2 {
		t.Errorf("an exited container still holds its port: %v", got)
	}

	fake.OnExec(func(id string, spec docker.ExecSpec) (docker.ExecResult, error) {
		return docker.ExecResult{ExitCode: 4, Output: "scripted " + spec.Command[0]}, nil
	})
	res, err := fake.Exec(ctx, "b", docker.ExecSpec{Command: []string{"pg_isready"}})
	if err != nil || res.ExitCode != 4 || res.Output != "scripted pg_isready" {
		t.Errorf("Exec = %+v, %v", res, err)
	}
	if got := fake.Execs("b"); len(got) != 1 || got[0].Command[0] != "pg_isready" {
		t.Errorf("Execs = %+v", got)
	}

	if err := fake.RemoveContainer(ctx, "b", true); err != nil {
		t.Fatal(err)
	}
	if got := fake.HostPorts(); len(got) != 1 || got[0] != 41000 {
		t.Errorf("host ports after removing b = %v", got)
	}
}

func TestFakeDockerLogsOfARemovedContainerEndTheFollow(t *testing.T) {
	ctx := context.Background()
	fake := testkit.NewFakeDocker()
	fake.AddImage("img")
	c, _ := fake.CreateContainer(ctx, docker.ContainerSpec{Name: "a", Image: "img"})
	fake.StartContainer(ctx, c.ID)
	r, err := fake.Logs(ctx, c.ID, docker.LogOptions{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	fake.Print(c.ID, "last words")
	fake.Exit(c.ID, 0)
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "last words\n" {
		t.Errorf("followed logs = %q, %v; the stream should end when the container does", got, err)
	}
}
