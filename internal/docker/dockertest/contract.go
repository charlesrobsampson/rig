// Package dockertest is the behavior every docker.Docker must have. The real client
// and the in-memory fake run the same suite.
package dockertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/docker"
)

// Subject is a Docker under test and the few things the suite cannot do through the
// interface alone.
type Subject struct {
	Docker docker.Docker
	// Image is a local image with a shell (busybox, alpine); containers run it.
	Image string
	// RunsProcesses is true for the real client: containers really execute their
	// command, so Exec output and exit codes can be checked.
	RunsProcesses bool
	// FreePort returns a host port nothing is listening on.
	FreePort func(t *testing.T) int
	// Exiting creates and starts a container whose process ends with the given code.
	Exiting func(t *testing.T, spec docker.ContainerSpec, code int) string
	// Print makes a running container write a line to its output.
	Print func(t *testing.T, id, line string)
	// MakeHealthy brings a container that has a healthcheck to healthy.
	MakeHealthy func(t *testing.T, id string)
	// Down makes the daemon unreachable until the test ends; nil when that cannot be done.
	Down func(t *testing.T)
}

var counter atomic.Int64

// env is one case's Docker. What its helpers create is removed when the case ends.
type env struct {
	Subject
	t    *testing.T
	nets []string
	ctrs []string
}

var ctx = context.Background()

func newEnv(t *testing.T, newSubject func(t *testing.T) Subject) *env {
	e := &env{Subject: newSubject(t), t: t}
	t.Cleanup(func() {
		for _, c := range e.ctrs {
			e.Docker.RemoveContainer(ctx, c, true)
		}
		for _, n := range e.nets {
			e.Docker.RemoveNetwork(ctx, n)
		}
	})
	return e
}

// name is unique across processes, so a leftover from a crashed run never collides.
func name(prefix string) string {
	return fmt.Sprintf("rigt-%s-%d-%d", prefix, os.Getpid(), counter.Add(1))
}

func (e *env) network(internal bool) docker.NetworkInfo {
	e.t.Helper()
	n, err := e.Docker.CreateNetwork(ctx, docker.NetworkSpec{Name: name("net"), Internal: internal, Labels: map[string]string{docker.LabelTicket: "t1", docker.LabelRole: "network"}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.nets = append(e.nets, n.ID)
	return n
}

func (e *env) spec(mutate func(*docker.ContainerSpec)) docker.ContainerSpec {
	s := docker.ContainerSpec{Name: name("c"), Image: e.Image, Command: []string{"sleep", "300"}, Labels: map[string]string{docker.LabelTicket: "t1", docker.LabelService: "api", docker.LabelRole: "service"}}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func (e *env) create(spec docker.ContainerSpec) docker.ContainerInfo {
	e.t.Helper()
	c, err := e.Docker.CreateContainer(ctx, spec)
	if err != nil {
		e.t.Fatalf("create %s: %v", spec.Name, err)
	}
	e.ctrs = append(e.ctrs, c.ID)
	return c
}

func (e *env) start(spec docker.ContainerSpec) docker.ContainerInfo {
	e.t.Helper()
	c := e.create(spec)
	if err := e.Docker.StartContainer(ctx, c.ID); err != nil {
		e.t.Fatalf("start %s: %v", spec.Name, err)
	}
	got, err := e.Docker.InspectContainer(ctx, c.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readAll(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Run executes the suite, asking newSubject for a fresh Docker for every case.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	t.Run("Ping", func(t *testing.T) {
		e := newEnv(t, newSubject)
		if err := e.Docker.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("networks", func(t *testing.T) {
		t.Run("create, inspect by name and by id, list by label, remove", func(t *testing.T) {
			e := newEnv(t, newSubject)
			n := e.network(false)
			if n.ID == "" || n.Internal || n.Labels[docker.LabelTicket] != "t1" {
				t.Fatalf("created = %+v", n)
			}
			byName, err1 := e.Docker.InspectNetwork(ctx, n.Name)
			byID, err2 := e.Docker.InspectNetwork(ctx, n.ID)
			if err1 != nil || err2 != nil || byName.ID != n.ID || byID.Name != n.Name {
				t.Fatalf("inspect: %+v %v / %+v %v", byName, err1, byID, err2)
			}
			listed, err := e.Docker.ListNetworks(ctx, map[string]string{docker.LabelTicket: "t1", docker.LabelRole: "network"})
			if err != nil || !hasNetwork(listed, n.ID) {
				t.Errorf("list by label = %+v, %v", listed, err)
			}
			if other, _ := e.Docker.ListNetworks(ctx, map[string]string{docker.LabelTicket: "no-such-ticket"}); hasNetwork(other, n.ID) {
				t.Error("a label that does not match listed the network")
			}
			if err := e.Docker.RemoveNetwork(ctx, n.Name); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Docker.InspectNetwork(ctx, n.ID); !errors.Is(err, docker.ErrNotFound) {
				t.Errorf("inspect after removal = %v, want ErrNotFound", err)
			}
		})
		t.Run("the internal flag is kept", func(t *testing.T) {
			e := newEnv(t, newSubject)
			internal, plain := e.network(true), e.network(false)
			for _, tc := range []struct {
				n    docker.NetworkInfo
				want bool
			}{{internal, true}, {plain, false}} {
				got, err := e.Docker.InspectNetwork(ctx, tc.n.ID)
				if err != nil || got.Internal != tc.want {
					t.Errorf("%s: Internal = %v (%v), want %v", tc.n.Name, got.Internal, err, tc.want)
				}
			}
		})
		t.Run("a duplicate name is refused", func(t *testing.T) {
			e := newEnv(t, newSubject)
			n := e.network(false)
			if _, err := e.Docker.CreateNetwork(ctx, docker.NetworkSpec{Name: n.Name}); !errors.Is(err, docker.ErrExists) {
				t.Errorf("duplicate = %v, want ErrExists", err)
			}
		})
		t.Run("an empty name is invalid", func(t *testing.T) {
			e := newEnv(t, newSubject)
			if _, err := e.Docker.CreateNetwork(ctx, docker.NetworkSpec{}); !errors.Is(err, docker.ErrInvalid) {
				t.Errorf("error = %v, want ErrInvalid", err)
			}
		})
	})

	t.Run("containers", func(t *testing.T) {
		t.Run("create then start then stop then remove", func(t *testing.T) {
			e := newEnv(t, newSubject)
			dir := t.TempDir()
			spec := e.spec(func(s *docker.ContainerSpec) {
				s.Env = map[string]string{"A": "1", "B": "two words"}
				s.Mounts = []docker.Mount{{Source: dir, Target: "/work"}, {Source: dir, Target: "/ro", ReadOnly: true}}
			})
			c := e.create(spec)
			if c.ID == "" || c.Name != spec.Name || c.State != docker.StateCreated || c.Health != docker.HealthNone {
				t.Fatalf("created = %+v", c)
			}
			if c.Labels[docker.LabelService] != "api" || c.Env["A"] != "1" || c.Env["B"] != "two words" {
				t.Errorf("labels or env lost: %+v", c)
			}
			if len(c.Mounts) != 2 || !hasMount(c.Mounts, docker.Mount{Source: dir, Target: "/work"}) || !hasMount(c.Mounts, docker.Mount{Source: dir, Target: "/ro", ReadOnly: true}) {
				t.Errorf("mounts = %+v, want %s at /work (rw) and /ro (ro)", c.Mounts, dir)
			}

			if err := e.Docker.StartContainer(ctx, spec.Name); err != nil { // by name
				t.Fatal(err)
			}
			eventually(t, "the container to run", func() bool {
				got, err := e.Docker.InspectContainer(ctx, c.ID)
				return err == nil && got.State == docker.StateRunning
			})
			listed, err := e.Docker.ListContainers(ctx, map[string]string{docker.LabelTicket: "t1", docker.LabelService: "api"})
			if err != nil || !hasContainer(listed, c.ID) {
				t.Errorf("list by label = %+v, %v", listed, err)
			}

			if err := e.Docker.RemoveContainer(ctx, c.ID, false); !errors.Is(err, docker.ErrInUse) {
				t.Errorf("removing a running container without force = %v, want ErrInUse", err)
			}
			if err := e.Docker.StopContainer(ctx, c.ID, time.Second); err != nil {
				t.Fatal(err)
			}
			if got, _ := e.Docker.InspectContainer(ctx, c.ID); got.State != docker.StateExited {
				t.Errorf("state after stop = %s", got.State)
			}
			if err := e.Docker.RemoveContainer(ctx, c.ID, false); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Docker.InspectContainer(ctx, c.ID); !errors.Is(err, docker.ErrNotFound) {
				t.Errorf("inspect after removal = %v, want ErrNotFound", err)
			}
		})
		t.Run("force removes a running container", func(t *testing.T) {
			e := newEnv(t, newSubject)
			c := e.start(e.spec(nil))
			if err := e.Docker.RemoveContainer(ctx, c.ID, true); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Docker.InspectContainer(ctx, c.ID); !errors.Is(err, docker.ErrNotFound) {
				t.Errorf("inspect = %v", err)
			}
		})
		t.Run("bad creates leave nothing behind", func(t *testing.T) {
			e := newEnv(t, newSubject)
			first := e.create(e.spec(nil))
			tests := []struct {
				name   string
				mutate func(*docker.ContainerSpec)
				want   error
			}{
				{"a duplicate name", func(s *docker.ContainerSpec) { s.Name = first.Name }, docker.ErrExists},
				{"no name", func(s *docker.ContainerSpec) { s.Name = "" }, docker.ErrInvalid},
				{"no image", func(s *docker.ContainerSpec) { s.Image = "" }, docker.ErrInvalid},
				{"an image that does not exist", func(s *docker.ContainerSpec) { s.Image = "rig-no-such-image:never" }, docker.ErrNotFound},
				{"a mount source that does not exist", func(s *docker.ContainerSpec) {
					s.Mounts = []docker.Mount{{Source: filepath.Join(t.TempDir(), "missing"), Target: "/x"}}
				}, docker.ErrInvalid},
				{"a network that does not exist", func(s *docker.ContainerSpec) { s.Network = "rig-no-such-network" }, docker.ErrNotFound},
			}
			for _, tt := range tests {
				spec := e.spec(tt.mutate)
				info, err := e.Docker.CreateContainer(ctx, spec)
				if err == nil {
					e.ctrs = append(e.ctrs, info.ID) // so a wrongly accepted create is still cleaned up
				}
				if !errors.Is(err, tt.want) {
					t.Errorf("%s: error = %v, want %v", tt.name, err, tt.want)
				}
			}
			listed, _ := e.Docker.ListContainers(ctx, map[string]string{docker.LabelTicket: "t1"})
			if len(listed) != 1 {
				t.Errorf("containers after the refused creates = %+v, want only the first", listed)
			}
		})
		t.Run("a container that exits keeps its code and its logs", func(t *testing.T) {
			e := newEnv(t, newSubject)
			id := e.Exiting(t, e.spec(nil), 7)
			e.ctrs = append(e.ctrs, id)
			eventually(t, "the container to exit", func() bool {
				got, err := e.Docker.InspectContainer(ctx, id)
				return err == nil && got.State == docker.StateExited
			})
			if got, _ := e.Docker.InspectContainer(ctx, id); got.ExitCode != 7 {
				t.Errorf("exit code = %d, want 7", got.ExitCode)
			}
		})
	})

	t.Run("networking", func(t *testing.T) {
		t.Run("containers join a network and peers resolve each other by name and alias", func(t *testing.T) {
			e := newEnv(t, newSubject)
			n := e.network(false)
			db := e.start(e.spec(func(s *docker.ContainerSpec) { s.Network, s.Aliases = n.Name, []string{"db"} }))
			api := e.start(e.spec(func(s *docker.ContainerSpec) { s.Network = n.Name }))

			for _, c := range []docker.ContainerInfo{db, api} {
				got := c.Networks[n.Name].Aliases
				if !contains(got, c.Name) {
					t.Errorf("%s aliases on the network = %v, want its own name among them", c.Name, got)
				}
			}
			if !contains(db.Networks[n.Name].Aliases, "db") {
				t.Errorf("aliases = %v, want db", db.Networks[n.Name].Aliases)
			}
			info, err := e.Docker.InspectNetwork(ctx, n.ID)
			if err != nil || !contains(info.Containers, db.ID) || !contains(info.Containers, api.ID) {
				t.Errorf("network members = %v (%v), want both containers", info.Containers, err)
			}
			if e.RunsProcesses {
				res, err := e.Docker.Exec(ctx, api.ID, docker.ExecSpec{Command: []string{"getent", "hosts", "db"}})
				if err != nil || res.ExitCode != 0 || res.Output == "" {
					t.Errorf("resolving db from the other container: %+v, %v", res, err)
				}
			}
		})
		t.Run("a container can be connected to a second network", func(t *testing.T) {
			e := newEnv(t, newSubject)
			first, second := e.network(false), e.network(true)
			c := e.start(e.spec(func(s *docker.ContainerSpec) { s.Network = first.Name }))
			if err := e.Docker.ConnectNetwork(ctx, c.ID, second.Name, []string{"proxy"}); err != nil {
				t.Fatal(err)
			}
			got, err := e.Docker.InspectContainer(ctx, c.ID)
			if err != nil || len(got.Networks) != 2 || !contains(got.Networks[second.Name].Aliases, "proxy") || !contains(got.Networks[second.Name].Aliases, c.Name) {
				t.Errorf("networks = %+v (%v), want both, with the alias and the container's own name on the second", got.Networks, err)
			}
			info, _ := e.Docker.InspectNetwork(ctx, second.ID)
			if !contains(info.Containers, c.ID) {
				t.Errorf("the second network's members = %v", info.Containers)
			}
			if err := e.Docker.ConnectNetwork(ctx, c.ID, second.Name, nil); !errors.Is(err, docker.ErrExists) {
				t.Errorf("connecting twice = %v, want ErrExists", err)
			}
			if err := e.Docker.ConnectNetwork(ctx, c.ID, "rig-no-such-network", nil); !errors.Is(err, docker.ErrNotFound) {
				t.Errorf("an unknown network = %v, want ErrNotFound", err)
			}
			if err := e.Docker.ConnectNetwork(ctx, "rigt-no-such-container", second.Name, nil); !errors.Is(err, docker.ErrNotFound) {
				t.Errorf("an unknown container = %v, want ErrNotFound", err)
			}
		})
		t.Run("a network with containers attached cannot be removed", func(t *testing.T) {
			e := newEnv(t, newSubject)
			n := e.network(false)
			c := e.start(e.spec(func(s *docker.ContainerSpec) { s.Network = n.Name }))
			if err := e.Docker.RemoveNetwork(ctx, n.ID); !errors.Is(err, docker.ErrInUse) {
				t.Fatalf("remove with a container attached = %v, want ErrInUse", err)
			}
			if err := e.Docker.RemoveContainer(ctx, c.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := e.Docker.RemoveNetwork(ctx, n.ID); err != nil {
				t.Errorf("remove after the container is gone: %v", err)
			}
		})
		t.Run("a host port can be held by one running container at a time", func(t *testing.T) {
			e := newEnv(t, newSubject)
			port := e.FreePort(t)
			bind := func(s *docker.ContainerSpec) { s.Ports = []docker.PortBinding{{Container: 8080, Host: port}} }
			first := e.start(e.spec(bind))
			if len(first.Ports) != 1 || first.Ports[0].Host != port || first.Ports[0].Container != 8080 {
				t.Fatalf("ports = %+v", first.Ports)
			}
			second := e.create(e.spec(bind))
			if err := e.Docker.StartContainer(ctx, second.ID); !errors.Is(err, docker.ErrInUse) {
				t.Fatalf("starting a second container on port %d = %v, want ErrInUse", port, err)
			}
			if got, _ := e.Docker.InspectContainer(ctx, second.ID); got.State == docker.StateRunning {
				t.Error("the refused container is running")
			}
			if err := e.Docker.RemoveContainer(ctx, first.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := e.Docker.StartContainer(ctx, second.ID); err != nil {
				t.Errorf("the port should be free again: %v", err)
			}
		})
		t.Run("host port 0 is chosen by Docker and reported", func(t *testing.T) {
			e := newEnv(t, newSubject)
			c := e.start(e.spec(func(s *docker.ContainerSpec) { s.Ports = []docker.PortBinding{{Container: 8080}} }))
			if len(c.Ports) != 1 || c.Ports[0].Host == 0 || c.Ports[0].Container != 8080 {
				t.Errorf("ports = %+v, want a chosen host port", c.Ports)
			}
		})
	})

	t.Run("logs", func(t *testing.T) {
		t.Run("output is readable, tail limits it, and it outlives the container", func(t *testing.T) {
			e := newEnv(t, newSubject)
			c := e.start(e.spec(nil))
			for _, line := range []string{"one", "two", "three"} {
				e.Print(t, c.ID, line)
			}
			eventually(t, "the output", func() bool {
				r, err := e.Docker.Logs(ctx, c.ID, docker.LogOptions{})
				return err == nil && strings.Contains(readAll(t, r), "three")
			})
			all, _ := e.Docker.Logs(ctx, c.ID, docker.LogOptions{})
			if got := readAll(t, all); !inOrder(got, "one", "two", "three") {
				t.Errorf("logs = %q", got)
			}
			tail, _ := e.Docker.Logs(ctx, c.ID, docker.LogOptions{Tail: 2})
			if got := readAll(t, tail); strings.Contains(got, "one") || !inOrder(got, "two", "three") {
				t.Errorf("tail 2 = %q", got)
			}
			if err := e.Docker.StopContainer(ctx, c.ID, time.Second); err != nil {
				t.Fatal(err)
			}
			after, err := e.Docker.Logs(ctx, c.ID, docker.LogOptions{})
			if err != nil || !strings.Contains(readAll(t, after), "three") {
				t.Errorf("logs after the container stopped: %v", err)
			}
		})
		t.Run("follow streams new lines until the context ends", func(t *testing.T) {
			e := newEnv(t, newSubject)
			c := e.start(e.spec(nil))
			e.Print(t, c.ID, "before")
			fctx, cancel := context.WithCancel(ctx)
			defer cancel()
			r, err := e.Docker.Logs(fctx, c.ID, docker.LogOptions{Follow: true})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			lines := make(chan string, 16)
			go func() {
				buf := make([]byte, 4096)
				var seen string
				for {
					n, err := r.Read(buf)
					seen += string(buf[:n])
					lines <- seen
					if err != nil {
						close(lines)
						return
					}
				}
			}()
			waitFor := func(want string) {
				t.Helper()
				deadline := time.After(20 * time.Second)
				for {
					select {
					case seen, ok := <-lines:
						if !ok {
							t.Fatalf("the stream ended before %q appeared", want)
						}
						if strings.Contains(seen, want) {
							return
						}
					case <-deadline:
						t.Fatalf("timed out waiting for %q", want)
					}
				}
			}
			waitFor("before")
			e.Print(t, c.ID, "after")
			waitFor("after")
			cancel()
			deadline := time.After(20 * time.Second)
			for {
				select {
				case _, ok := <-lines:
					if !ok {
						return
					}
				case <-deadline:
					t.Fatal("the stream did not end when the context was cancelled")
				}
			}
		})
	})

	t.Run("health", func(t *testing.T) {
		e := newEnv(t, newSubject)
		plain := e.start(e.spec(nil))
		if plain.Health != docker.HealthNone {
			t.Errorf("a container without a healthcheck has health %q, want none", plain.Health)
		}
		checked := e.start(e.spec(func(s *docker.ContainerSpec) {
			s.Healthcheck = &docker.Healthcheck{Command: []string{"true"}, Interval: time.Second, Timeout: time.Second, Retries: 3}
		}))
		if checked.Health != docker.HealthStarting && checked.Health != docker.HealthHealthy {
			t.Errorf("health right after start = %q, want starting", checked.Health)
		}
		e.MakeHealthy(t, checked.ID)
		eventually(t, "the container to be healthy", func() bool {
			got, err := e.Docker.InspectContainer(ctx, checked.ID)
			return err == nil && got.Health == docker.HealthHealthy
		})
	})

	t.Run("exec", func(t *testing.T) {
		e := newEnv(t, newSubject)
		c := e.start(e.spec(nil))
		if e.RunsProcesses {
			res, err := e.Docker.Exec(ctx, c.ID, docker.ExecSpec{Command: []string{"sh", "-c", `echo "$GREETING from $(pwd)"; exit 3`}, Env: map[string]string{"GREETING": "hi"}, WorkDir: "/tmp"})
			if err != nil || res.ExitCode != 3 || strings.TrimSpace(res.Output) != "hi from /tmp" {
				t.Errorf("exec = %+v, %v; want exit 3 and %q", res, err, "hi from /tmp")
			}
		} else if res, err := e.Docker.Exec(ctx, c.ID, docker.ExecSpec{Command: []string{"true"}}); err != nil || res.ExitCode != 0 {
			t.Errorf("exec = %+v, %v", res, err)
		}
		if _, err := e.Docker.Exec(ctx, c.ID, docker.ExecSpec{}); !errors.Is(err, docker.ErrInvalid) {
			t.Errorf("exec with no command = %v, want ErrInvalid", err)
		}
		if err := e.Docker.StopContainer(ctx, c.ID, time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Docker.Exec(ctx, c.ID, docker.ExecSpec{Command: []string{"true"}}); !errors.Is(err, docker.ErrNotRunning) {
			t.Errorf("exec in a stopped container = %v, want ErrNotRunning", err)
		}
	})

	t.Run("images", func(t *testing.T) {
		e := newEnv(t, newSubject)
		ctxDir := t.TempDir()
		spec := docker.BuildSpec{Tag: "rig-test-build:latest", ContextDir: ctxDir, Labels: map[string]string{"rig.test": "1"}}
		if err := e.Docker.BuildImage(ctx, spec); !errors.Is(err, docker.ErrInvalid) {
			t.Errorf("a build with no Dockerfile = %v, want ErrInvalid", err)
		}
		if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM "+e.Image+"\nCOPY hello.txt /hello.txt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(ctxDir, "hello.txt"), []byte("hi\n"), 0o600)
		if err := e.Docker.BuildImage(ctx, spec); err != nil {
			t.Fatalf("build: %v", err)
		}
		if ok, err := e.Docker.ImageExists(ctx, spec.Tag); err != nil || !ok {
			t.Errorf("ImageExists(%s) = %v, %v", spec.Tag, ok, err)
		}
		if ok, err := e.Docker.ImageExists(ctx, "rig-no-such-image:never"); err != nil || ok {
			t.Errorf("ImageExists(missing) = %v, %v", ok, err)
		}
		id, err := e.Docker.ImageID(ctx, spec.Tag)
		if err != nil || id == "" {
			t.Errorf("ImageID(%s) = %q, %v", spec.Tag, id, err)
		}
		if _, err := e.Docker.ImageID(ctx, "rig-no-such-image:never"); !errors.Is(err, docker.ErrNotFound) {
			t.Errorf("ImageID(missing) = %v, want ErrNotFound", err)
		}
		os.WriteFile(filepath.Join(ctxDir, "hello.txt"), []byte("changed\n"), 0o600)
		if err := e.Docker.BuildImage(ctx, spec); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if again, err := e.Docker.ImageID(ctx, spec.Tag); err != nil || again == id {
			t.Errorf("ImageID after the tag was rebuilt with different content = %q (%v), want a new id than %q", again, err, id)
		}
		os.WriteFile(filepath.Join(ctxDir, "Dockerfile.other"), []byte("FROM "+e.Image+"\n"), 0o600)
		if err := e.Docker.BuildImage(ctx, docker.BuildSpec{Tag: "rig-test-build:other", ContextDir: ctxDir, Dockerfile: "Dockerfile.other"}); err != nil {
			t.Errorf("build with a named Dockerfile: %v", err)
		}
		if err := e.Docker.BuildImage(ctx, docker.BuildSpec{Tag: "", ContextDir: ctxDir}); !errors.Is(err, docker.ErrInvalid) {
			t.Errorf("build with no tag = %v, want ErrInvalid", err)
		}
	})

	t.Run("everything on an unknown id is not found", func(t *testing.T) {
		e := newEnv(t, newSubject)
		const missing = "rigt-does-not-exist"
		checks := map[string]error{}
		_, checks["InspectNetwork"] = e.Docker.InspectNetwork(ctx, missing)
		checks["RemoveNetwork"] = e.Docker.RemoveNetwork(ctx, missing)
		_, checks["InspectContainer"] = e.Docker.InspectContainer(ctx, missing)
		checks["StartContainer"] = e.Docker.StartContainer(ctx, missing)
		checks["StopContainer"] = e.Docker.StopContainer(ctx, missing, time.Second)
		checks["RemoveContainer"] = e.Docker.RemoveContainer(ctx, missing, true)
		_, checks["Exec"] = e.Docker.Exec(ctx, missing, docker.ExecSpec{Command: []string{"true"}})
		_, checks["Logs"] = e.Docker.Logs(ctx, missing, docker.LogOptions{})
		for op, err := range checks {
			if !errors.Is(err, docker.ErrNotFound) {
				t.Errorf("%s = %v, want ErrNotFound", op, err)
			}
		}
	})

	t.Run("a cancelled context stops a call", func(t *testing.T) {
		e := newEnv(t, newSubject)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.Docker.CreateNetwork(cancelled, docker.NetworkSpec{Name: name("net")}); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if _, err := e.Docker.ListContainers(cancelled, nil); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})

	t.Run("an unreachable daemon is reported as unavailable", func(t *testing.T) {
		e := newEnv(t, newSubject)
		if e.Down == nil {
			t.Skip("cannot make this daemon unreachable")
		}
		e.Down(t)
		if err := e.Docker.Ping(ctx); !errors.Is(err, docker.ErrUnavailable) {
			t.Errorf("Ping = %v, want ErrUnavailable", err)
		}
		if _, err := e.Docker.ListContainers(ctx, nil); !errors.Is(err, docker.ErrUnavailable) {
			t.Errorf("ListContainers = %v, want ErrUnavailable", err)
		}
	})
}

func hasNetwork(list []docker.NetworkInfo, id string) bool {
	for _, n := range list {
		if n.ID == id {
			return true
		}
	}
	return false
}

func hasContainer(list []docker.ContainerInfo, id string) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

func hasMount(list []docker.Mount, want docker.Mount) bool {
	want.Source = resolve(want.Source)
	for _, m := range list {
		m.Source = resolve(m.Source)
		if reflect.DeepEqual(m, want) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func inOrder(s string, parts ...string) bool {
	at := 0
	for _, p := range parts {
		i := strings.Index(s[at:], p)
		if i < 0 {
			return false
		}
		at += i + len(p)
	}
	return true
}

// resolve follows symlinks: a mount source may be reported by its real path on
// platforms whose temp directories are links (macOS /var).
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
