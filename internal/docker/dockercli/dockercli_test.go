package dockercli_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/dockercli"
)

var ctx = context.Background()

// script is a Runner that answers calls by their first words and records them.
type script struct {
	calls   []dockercli.Call
	answers map[string]dockercli.Result
}

func (s *script) run(_ context.Context, c dockercli.Call) (dockercli.Result, error) {
	s.calls = append(s.calls, c)
	key := strings.Join(c.Args[:min(2, len(c.Args))], " ")
	if r, ok := s.answers[key]; ok {
		return r, nil
	}
	return dockercli.Result{}, nil
}

func (s *script) last() dockercli.Call { return s.calls[len(s.calls)-1] }

func newClient(answers map[string]dockercli.Result) (*dockercli.Client, *script) {
	s := &script{answers: answers}
	return dockercli.New(dockercli.Options{Runner: s.run}), s
}

func fail(stderr string) dockercli.Result {
	return dockercli.Result{ExitCode: 1, Stderr: stderr, Output: stderr}
}

func TestDockerErrorsBecomeTheRightSentinels(t *testing.T) {
	tests := []struct {
		stderr string
		want   error
	}{
		{"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", docker.ErrUnavailable},
		{"failed to connect to the docker API at unix:///x.sock; check if the path is correct", docker.ErrUnavailable},
		{"error during connect: Get \"http://%2F%2F.%2Fpipe%2Fdocker_engine\": open", docker.ErrUnavailable},
		{"Error response from daemon: network rigt-x not found", docker.ErrNotFound},
		{"Error response from daemon: No such container: abc", docker.ErrNotFound},
		{"Error: No such network: abc", docker.ErrNotFound},
		{"Error response from daemon: pull access denied for nothing, repository does not exist or may require 'docker login'", docker.ErrNotFound},
		{"Error response from daemon: manifest for x:never not found: manifest unknown", docker.ErrNotFound},
		{"Error response from daemon: network with name rigt-x already exists", docker.ErrExists},
		{"Error response from daemon: Conflict. The container name \"/rigt\" is already in use by container \"abc\".", docker.ErrExists},
		{"Error response from daemon: driver failed programming external connectivity: Bind for 127.0.0.1:41000 failed: port is already allocated", docker.ErrInUse},
		{"Error response from daemon: network rigt-x has active endpoints", docker.ErrInUse},
		{"Error response from daemon: cannot remove container \"abc\": container is running: stop the container before removing or force remove", docker.ErrInUse},
		{"Error response from daemon: container abc is not running", docker.ErrNotRunning},
	}
	for _, tt := range tests {
		t.Run(tt.stderr[:min(50, len(tt.stderr))], func(t *testing.T) {
			c, _ := newClient(map[string]dockercli.Result{"network rm": fail(tt.stderr)})
			err := c.RemoveNetwork(ctx, "x")
			if !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
			if strings.Count(err.Error(), "\n") > 0 {
				t.Errorf("the error should be one line: %q", err)
			}
		})
	}
	c, _ := newClient(map[string]dockercli.Result{"network rm": fail("some new docker message")})
	if err := c.RemoveNetwork(ctx, "x"); err == nil || !strings.Contains(err.Error(), "some new docker message") {
		t.Errorf("an unrecognized failure should carry docker's text: %v", err)
	}
}

func TestUnavailableMessagesTellTheUserWhatToDo(t *testing.T) {
	c, _ := newClient(map[string]dockercli.Result{"version --format": fail("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")})
	err := c.Ping(ctx)
	if !errors.Is(err, docker.ErrUnavailable) || !strings.Contains(err.Error(), "start Docker") {
		t.Errorf("Ping = %v", err)
	}
}

func TestAMissingDockerBinaryIsUnavailable(t *testing.T) {
	c := dockercli.New(dockercli.Options{Binary: "no-such-docker-binary-xyz"})
	err := c.Ping(ctx)
	if !errors.Is(err, docker.ErrUnavailable) || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("Ping = %v", err)
	}
}

func TestCreateNetworkArguments(t *testing.T) {
	c, s := newClient(map[string]dockercli.Result{"network inspect": {Stdout: `[{"Id":"n1","Name":"rig-demo","Internal":true,"Labels":{"rig.ticket":"demo"}}]`}})
	n, err := c.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-demo", Internal: true, Labels: docker.Labels("demo", "", docker.RoleNetwork)})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"network", "create", "--internal", "--label", "rig.role=network", "--label", "rig.ticket=demo", "rig-demo"}
	if !reflect.DeepEqual(s.calls[0].Args, want) {
		t.Errorf("args = %q, want %q", s.calls[0].Args, want)
	}
	if n.ID != "n1" || !n.Internal || n.Labels["rig.ticket"] != "demo" {
		t.Errorf("info = %+v", n)
	}
	cPlain, sPlain := newClient(nil)
	cPlain.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-demo"})
	if contains(sPlain.calls[0].Args, "--internal") {
		t.Error("--internal passed for a plain network")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestCreateContainerArguments(t *testing.T) {
	src := t.TempDir()
	c, s := newClient(map[string]dockercli.Result{"inspect --type": {Stdout: `[{"Id":"c1","Name":"/web","State":{"Status":"created"},"Config":{"Image":"img"}}]`}, "network inspect": {Stdout: `[{"Id":"n1","Name":"rig-demo"}]`}})
	_, err := c.CreateContainer(ctx, docker.ContainerSpec{
		Name: "web", Image: "img", Command: []string{"npm", "run", "dev"},
		Env:     map[string]string{"B": "2", "A": "secret-sentinel"},
		Labels:  docker.Labels("demo", "web", docker.RoleService),
		WorkDir: "/work", User: "501:20",
		Mounts:  []docker.Mount{{Source: src, Target: "/work"}, {Source: src, Target: "/ro", ReadOnly: true}},
		Ports:   []docker.PortBinding{{Container: 3000, Host: 41000}, {Container: 9000}},
		Network: "rig-demo", Aliases: []string{"api", "web.local"},
		Healthcheck: &docker.Healthcheck{Command: []string{"curl", "-f", "http://localhost:3000/it's"}, Interval: 5 * time.Second, Timeout: 2 * time.Second, Retries: 4},
		TTY:         true, Stdin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var create dockercli.Call
	for _, call := range s.calls {
		if call.Args[0] == "create" {
			create = call
		}
	}
	args := strings.Join(create.Args, " ")
	for _, want := range []string{
		"create --name web",
		"--label rig.role=service --label rig.service=web --label rig.ticket=demo",
		"--env A --env B",
		"--mount type=bind,source=" + src + ",target=/work --mount type=bind,source=" + src + ",target=/ro,readonly",
		"--publish 127.0.0.1:41000:3000 --publish 127.0.0.1::9000",
		"--network rig-demo --network-alias api --network-alias web.local",
		"--workdir /work", "--user 501:20", "--tty", "--interactive",
		"--health-interval 5s --health-timeout 2s --health-retries 4",
		"--health-cmd 'curl' '-f' 'http://localhost:3000/it'\\''s'",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if !strings.HasSuffix(args, " img npm run dev") {
		t.Errorf("the image and command must come last: %s", args)
	}
	if strings.Contains(args, "secret-sentinel") {
		t.Error("an env value is on the command line")
	}
	if create.Env["A"] != "secret-sentinel" || create.Env["B"] != "2" {
		t.Errorf("env values must travel in the process environment: %v", create.Env)
	}
}

func TestCreateContainerRefusesBadSpecsBeforeCallingDocker(t *testing.T) {
	tmp := t.TempDir()
	tests := []struct {
		name string
		spec docker.ContainerSpec
	}{
		{"no name", docker.ContainerSpec{Image: "img"}},
		{"no image", docker.ContainerSpec{Name: "x"}},
		{"a relative mount source", docker.ContainerSpec{Name: "x", Image: "i", Mounts: []docker.Mount{{Source: "rel", Target: "/x"}}}},
		{"a relative mount target", docker.ContainerSpec{Name: "x", Image: "i", Mounts: []docker.Mount{{Source: tmp, Target: "x"}}}},
		{"a missing mount source", docker.ContainerSpec{Name: "x", Image: "i", Mounts: []docker.Mount{{Source: filepath.Join(tmp, "gone"), Target: "/x"}}}},
		{"a comma in a mount path", docker.ContainerSpec{Name: "x", Image: "i", Mounts: []docker.Mount{{Source: tmp, Target: "/x,readonly=false"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newClient(nil)
			if _, err := c.CreateContainer(ctx, tt.spec); !errors.Is(err, docker.ErrInvalid) {
				t.Errorf("error = %v, want ErrInvalid", err)
			}
			if len(s.calls) != 0 {
				t.Errorf("docker was called: %v", s.calls[0].Args)
			}
		})
	}
}

func TestCreateContainerChecksTheNetworkFirst(t *testing.T) {
	c, s := newClient(map[string]dockercli.Result{"network inspect": fail("Error response from daemon: network nope not found")})
	_, err := c.CreateContainer(ctx, docker.ContainerSpec{Name: "x", Image: "i", Network: "nope"})
	if !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("error = %v", err)
	}
	for _, call := range s.calls {
		if call.Args[0] == "create" {
			t.Error("docker create ran although the network does not exist")
		}
	}
}

func TestInspectParsesWhatDockerReports(t *testing.T) {
	const inspect = `[{
	  "Id": "abc123", "Name": "/rig-demo-api",
	  "State": {"Status": "running", "ExitCode": 0, "Health": {"Status": "healthy"}},
	  "Config": {"Image": "node:22", "Env": ["PATH=/bin", "PORT=3000", "EMPTY="], "Labels": {"rig.ticket": "demo"}},
	  "Mounts": [{"Type": "bind", "Source": "/host/work", "Destination": "/work", "RW": true}, {"Type": "bind", "Source": "/host/objects", "Destination": "/objects", "RW": false}, {"Type": "volume", "Source": "v", "Destination": "/v", "RW": true}],
	  "HostConfig": {"PortBindings": {"3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": ""}]}},
	  "NetworkSettings": {"Ports": {"3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "41234"}], "9229/tcp": null}, "Networks": {"rig-demo": {"Aliases": ["api", "abc123"]}}}
	}, {
	  "Id": "def456", "Name": "/stopped",
	  "State": {"Status": "exited", "ExitCode": 137},
	  "Config": {"Image": "img"},
	  "HostConfig": {"PortBindings": {"80/tcp": [{"HostPort": "8080"}]}},
	  "NetworkSettings": {"Ports": {}, "Networks": {}}
	}, {
	  "Id": "ghi789", "Name": "/unhealthy",
	  "State": {"Status": "running", "Health": {"Status": "unhealthy"}},
	  "Config": {"Image": "img"}, "NetworkSettings": {}
	}, {
	  "Id": "jkl", "Name": "/new", "State": {"Status": "created", "ExitCode": 0, "Health": {"Status": "starting"}}, "Config": {"Image": "img"}, "NetworkSettings": {}
	}]`
	c, _ := newClient(map[string]dockercli.Result{"ps --all": {Stdout: "abc123\ndef456\nghi789\njkl\n"}, "inspect --type": {Stdout: inspect}})
	got, err := c.ListContainers(ctx, map[string]string{"rig.ticket": "demo"})
	if err != nil || len(got) != 4 {
		t.Fatalf("ListContainers = %v, %v", got, err)
	}
	byName := map[string]docker.ContainerInfo{}
	for _, g := range got {
		byName[g.Name] = g
	}

	api := byName["rig-demo-api"]
	if api.ID != "abc123" || api.Image != "node:22" || api.State != docker.StateRunning || api.Health != docker.HealthHealthy {
		t.Errorf("api = %+v", api)
	}
	if api.Env["PORT"] != "3000" || api.Env["EMPTY"] != "" || api.Labels["rig.ticket"] != "demo" {
		t.Errorf("env or labels = %v %v", api.Env, api.Labels)
	}
	if !reflect.DeepEqual(api.Mounts, []docker.Mount{{Source: "/host/work", Target: "/work"}, {Source: "/host/objects", Target: "/objects", ReadOnly: true}}) {
		t.Errorf("only bind mounts, with read-only kept: %+v", api.Mounts)
	}
	if !reflect.DeepEqual(api.Ports, []docker.PortBinding{{Container: 3000, Host: 41234}}) {
		t.Errorf("a running container reports the real host port: %+v", api.Ports)
	}
	if !reflect.DeepEqual(api.Networks["rig-demo"].Aliases, []string{"rig-demo-api", "api", "abc123"}) {
		t.Errorf("aliases = %v, want the container's own name added", api.Networks["rig-demo"].Aliases)
	}

	stopped := byName["stopped"]
	if stopped.State != docker.StateExited || stopped.ExitCode != 137 || stopped.Health != docker.HealthNone {
		t.Errorf("stopped = %+v", stopped)
	}
	if !reflect.DeepEqual(stopped.Ports, []docker.PortBinding{{Container: 80, Host: 8080}}) {
		t.Errorf("a stopped container reports what it will bind: %+v", stopped.Ports)
	}
	if byName["unhealthy"].Health != docker.HealthUnhealthy || byName["new"].State != docker.StateCreated || byName["new"].Health != docker.HealthStarting {
		t.Errorf("unhealthy = %+v, new = %+v", byName["unhealthy"], byName["new"])
	}
}

func TestListFiltersByEveryLabelAndSkipsInspectWhenEmpty(t *testing.T) {
	c, s := newClient(map[string]dockercli.Result{"ps --all": {Stdout: ""}})
	got, err := c.ListContainers(ctx, map[string]string{"rig.ticket": "demo", "rig.service": "api"})
	if err != nil || len(got) != 0 {
		t.Fatalf("ListContainers = %v, %v", got, err)
	}
	args := strings.Join(s.calls[0].Args, " ")
	if !strings.Contains(args, "--filter label=rig.service=api --filter label=rig.ticket=demo") {
		t.Errorf("args = %s", args)
	}
	if len(s.calls) != 1 {
		t.Errorf("made %d calls; with nothing listed there is nothing to inspect", len(s.calls))
	}
}

func TestForceRemoveOfAMissingContainerIsNotFound(t *testing.T) {
	c, s := newClient(map[string]dockercli.Result{"inspect --type": fail("Error: No such object: x")})
	if err := c.RemoveContainer(ctx, "x", true); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("error = %v", err)
	}
	for _, call := range s.calls {
		if call.Args[0] == "rm" {
			t.Error("rm ran for a container that does not exist")
		}
	}
}

func TestExecPassesWhatTheCommandPrintedAndItsExitCode(t *testing.T) {
	c, s := newClient(map[string]dockercli.Result{"exec --env": {ExitCode: 3, Stderr: "boom\n", Output: "out\nboom\n"}})
	res, err := c.Exec(ctx, "web", docker.ExecSpec{Command: []string{"sh", "-c", "x"}, Env: map[string]string{"TOKEN": "tok-sentinel"}, User: "1:1", WorkDir: "/w"})
	if err != nil || res.ExitCode != 3 || res.Output != "out\nboom\n" {
		t.Errorf("Exec = %+v, %v; a failing command is a result, not an error", res, err)
	}
	args := strings.Join(s.calls[0].Args, " ")
	if args != "exec --env TOKEN --user 1:1 --workdir /w web sh -c x" || strings.Contains(args, "tok-sentinel") || s.calls[0].Env["TOKEN"] != "tok-sentinel" {
		t.Errorf("args = %s, env = %v", args, s.calls[0].Env)
	}

	for stderr, want := range map[string]error{
		"Error response from daemon: container abc is not running":                        docker.ErrNotRunning,
		"Error response from daemon: No such container: abc":                              docker.ErrNotFound,
		"Cannot connect to the Docker daemon at unix:///x. Is the docker daemon running?": docker.ErrUnavailable,
	} {
		c, _ := newClient(map[string]dockercli.Result{"exec web": fail(stderr)})
		if _, err := c.Exec(ctx, "web", docker.ExecSpec{Command: []string{"true"}}); !errors.Is(err, want) {
			t.Errorf("%q: error = %v, want %v", stderr, err, want)
		}
	}
}

func TestBuildArgumentsAndFailureTail(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile.dev"), []byte("FROM scratch\n"), 0o600)
	c, s := newClient(nil)
	err := c.BuildImage(ctx, docker.BuildSpec{Tag: "rig-demo-api:1", ContextDir: dir, Dockerfile: "Dockerfile.dev", Args: map[string]string{"B": "2", "A": "1"}, Labels: map[string]string{"rig.ticket": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "--tag", "rig-demo-api:1", "--file", filepath.Join(dir, "Dockerfile.dev"), "--label", "rig.ticket=demo", "--build-arg", "A=1", "--build-arg", "B=2", dir}
	if !reflect.DeepEqual(s.calls[0].Args, want) {
		t.Errorf("args = %q\nwant   %q", s.calls[0].Args, want)
	}

	var out strings.Builder
	for i := 1; i <= 50; i++ {
		out.WriteString("step line " + string(rune('A'+i%26)) + "\n")
	}
	out.WriteString("ERROR: failed to solve: process \"/bin/sh -c make\" did not complete successfully\n")
	c, _ = newClient(map[string]dockercli.Result{"build --tag": {ExitCode: 1, Stderr: "ERROR", Output: out.String()}})
	err = c.BuildImage(ctx, docker.BuildSpec{Tag: "t", ContextDir: dir, Dockerfile: "Dockerfile.dev"})
	if err == nil || !strings.Contains(err.Error(), "did not complete successfully") || strings.Count(err.Error(), "\n") > 21 {
		t.Errorf("a failed build should end with the last lines of its output: %v", err)
	}
}

func TestImageExistsDistinguishesMissingFromUnreachable(t *testing.T) {
	c, _ := newClient(map[string]dockercli.Result{"image inspect": fail("Error response from daemon: No such image: x")})
	if ok, err := c.ImageExists(ctx, "x"); ok || err != nil {
		t.Errorf("missing image = %v, %v", ok, err)
	}
	c, _ = newClient(map[string]dockercli.Result{"image inspect": fail("Cannot connect to the Docker daemon at unix:///x. Is the docker daemon running?")})
	if _, err := c.ImageExists(ctx, "x"); !errors.Is(err, docker.ErrUnavailable) {
		t.Errorf("unreachable daemon = %v, want ErrUnavailable", err)
	}
}

func TestLogs(t *testing.T) {
	c, s := newClient(map[string]dockercli.Result{
		"inspect --type": {Stdout: `[{"Id":"c1","Name":"/web","State":{"Status":"exited"},"Config":{"Image":"i"}}]`},
		"logs --tail":    {Output: "two\nthree\n"},
	})
	r, err := c.Logs(ctx, "web", docker.LogOptions{Tail: 2})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "two\nthree\n" {
		t.Errorf("logs = %q", b)
	}
	if got := strings.Join(s.last().Args, " "); got != "logs --tail 2 web" {
		t.Errorf("args = %s", got)
	}

	missing, _ := newClient(map[string]dockercli.Result{"inspect --type": fail("Error: No such object: web")})
	if _, err := missing.Logs(ctx, "web", docker.LogOptions{}); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("logs of a missing container = %v", err)
	}
}

func TestFollowingLogsUsesTheStreamer(t *testing.T) {
	var gotArgs []string
	streamed := io.NopCloser(strings.NewReader("line\n"))
	c := dockercli.New(dockercli.Options{
		Runner: func(context.Context, dockercli.Call) (dockercli.Result, error) {
			return dockercli.Result{Stdout: `[{"Id":"c1","Name":"/web","State":{"Status":"running"},"Config":{"Image":"i"}}]`}, nil
		},
		Streamer: func(_ context.Context, args []string) (io.ReadCloser, error) { gotArgs = args; return streamed, nil },
	})
	r, err := c.Logs(ctx, "web", docker.LogOptions{Follow: true, Tail: 5})
	if err != nil || r != streamed {
		t.Fatalf("Logs = %v, %v", r, err)
	}
	if got := strings.Join(gotArgs, " "); got != "logs --tail 5 --follow web" {
		t.Errorf("args = %s", got)
	}
}

func TestTheContextEndsACall(t *testing.T) {
	c := dockercli.New(dockercli.Options{Runner: func(ctx context.Context, _ dockercli.Call) (dockercli.Result, error) {
		t.Error("docker was run with a cancelled context")
		return dockercli.Result{}, nil
	}})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.StartContainer(cancelled, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v", err)
	}
}

func TestLabelsHelper(t *testing.T) {
	want := map[string]string{"rig.ticket": "demo", "rig.role": "dependency", "rig.service": "db"}
	if got := docker.Labels("demo", "db", docker.RoleDependency); !reflect.DeepEqual(got, want) {
		t.Errorf("Labels = %v", got)
	}
	if got := docker.Labels("demo", "", docker.RoleProxy); !reflect.DeepEqual(got, map[string]string{"rig.ticket": "demo", "rig.role": "proxy"}) {
		t.Errorf("Labels without a service = %v", got)
	}
}

func TestImageID(t *testing.T) {
	c, s := newClient(map[string]dockercli.Result{"image inspect": {Stdout: "sha256:abc123\n"}})
	id, err := c.ImageID(ctx, "golang:1.24")
	if err != nil || id != "sha256:abc123" {
		t.Errorf("ImageID = %q, %v", id, err)
	}
	if got := strings.Join(s.last().Args, " "); got != "image inspect --format {{.Id}} golang:1.24" {
		t.Errorf("args = %s", got)
	}
	missing, _ := newClient(map[string]dockercli.Result{"image inspect": fail("Error response from daemon: No such image: x")})
	if _, err := missing.ImageID(ctx, "x"); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("a missing image = %v, want ErrNotFound", err)
	}
	empty, _ := newClient(map[string]dockercli.Result{"image inspect": {Stdout: "\n"}})
	if _, err := empty.ImageID(ctx, "x"); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("an empty answer = %v, want ErrNotFound", err)
	}
}
