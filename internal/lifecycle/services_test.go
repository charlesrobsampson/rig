package lifecycle_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

type probeFn struct {
	mu      sync.Mutex
	healthy bool
	urls    []string
}

func (p *probeFn) probe(_ context.Context, url string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = append(p.urls, url)
	return p.healthy
}

func (p *probeFn) set(v bool) { p.mu.Lock(); p.healthy = v; p.mu.Unlock() }

// serviceFixture is a Docker-backed lifecycle on the real clock with tiny timings.
func serviceFixture(t *testing.T, mutate func(*lifecycle.Options)) (*fixture, *testkit.FakeDocker, *probeFn) {
	t.Helper()
	f := newFixture(t, domain.SandboxContainer)
	dk := testkit.NewFakeDocker()
	probe := &probeFn{healthy: true}
	opts := lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Docker: dk, HealthProbe: probe.probe, EgressBinary: fakeProxyBinary,
		Global:              config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, BranchPrefix: "rig/"}},
		ServiceStartTimeout: 300 * time.Millisecond, HealthPoll: time.Millisecond, Settle: 20 * time.Millisecond, StopGrace: time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	f.lc = lifecycle.New(opts)
	return f, dk, probe
}

// withService turns svc1 of a created ticket into one that runs in a container.
func withService(t *testing.T, path string, repo *testkit.Repo, dockerfile bool, extra string) {
	t.Helper()
	if dockerfile {
		repo.Commit("Dockerfile", "FROM scratch\n", "add a Dockerfile")
	}
	yamlPath := filepath.Join(path, "ticket.yaml")
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(raw), "      branch: rig/", "      "+extra+"\n      branch: rig/", 1)
	if err := os.WriteFile(yamlPath, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
}

const apiService = `build: {dockerfile: Dockerfile}
      start: "node server.js"
      internal_port: 8080
      healthcheck: /healthz`

func containerByService(res api.TicketStartResult) map[string]api.ServiceContainerInfo {
	out := map[string]api.ServiceContainerInfo{}
	for _, c := range res.Containers {
		out[c.Service] = c
	}
	return out
}

func TestAServiceRunsInAContainerWithItsCheckoutEnvAndPort(t *testing.T) {
	f, dk, probe := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, apiService)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("env: {DEBUG: \"true\"}\n")...), 0o600)

	res := f.mustStart("demo", domain.SandboxContainer)
	c := containerByService(res)["svc1"]
	if !c.Running || c.Error != "" || c.Name != "rig-demo-svc1" || c.HostPort < 41000 || c.URL != "http://localhost:"+itoa(c.HostPort) {
		t.Fatalf("container = %+v", c)
	}

	got, err := dk.InspectContainer(ctx, "rig-demo-svc1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != docker.StateRunning || got.Image != "rig-demo-svc1:latest" {
		t.Errorf("container = %+v", got)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Source != res.Checkouts[0].Path || got.Mounts[0].Target != "/work" || got.Mounts[0].ReadOnly {
		t.Errorf("mounts = %+v, want the checkout at /work read-write", got.Mounts)
	}
	if got.Env["DEBUG"] != "true" || got.Env["RIG_TICKET"] != "demo" {
		t.Errorf("env = %v", got.Env)
	}
	if len(got.Ports) != 1 || got.Ports[0].Container != 8080 || got.Ports[0].Host != c.HostPort {
		t.Errorf("ports = %+v", got.Ports)
	}
	if got.Labels["rig.ticket"] != "demo" || got.Labels["rig.service"] != "svc1" || got.Labels["rig.role"] != "service" {
		t.Errorf("labels = %v", got.Labels)
	}
	if aliases := got.Networks["rig-demo"].Aliases; !contains(aliases, "svc1") {
		t.Errorf("aliases = %v; peers must reach it as svc1", aliases)
	}
	spawned := dk.ContainerSpec("rig-demo-svc1")
	if strings.Join(spawned.Command, " ") != "sh -c node server.js" || spawned.WorkDir != "/work" {
		t.Errorf("command = %q, workdir %q", spawned.Command, spawned.WorkDir)
	}
	if len(probe.urls) == 0 || probe.urls[0] != c.URL+"/healthz" {
		t.Errorf("healthcheck probed %v, want %s/healthz", probe.urls, c.URL)
	}

	st := f.state("demo").Services["svc1"]
	if st.Status != domain.ServiceRunning || st.ContainerID != got.ID || st.HostPort != c.HostPort || st.Error != "" {
		t.Errorf("state = %+v", st)
	}
	if built, _ := dk.ImageExists(ctx, "rig-demo-svc1:latest"); !built {
		t.Error("the service image was not built")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func fmtInt(n int) string { return strconv.Itoa(n) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestServiceStatusEventsAreAnnounced(t *testing.T) {
	f, _, _ := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	f.events()
	f.mustStart("demo", domain.SandboxContainer)
	var statuses []domain.ServiceStatus
	for _, ev := range f.events() {
		if ev.Type == api.EventServiceStatus && ev.Service == "svc1" {
			var d api.ServiceStatusData
			json.Unmarshal(ev.Data, &d)
			statuses = append(statuses, d.Status)
		}
	}
	if len(statuses) != 2 || statuses[0] != domain.ServiceProvisioned || statuses[1] != domain.ServiceRunning {
		t.Errorf("status events = %v, want provisioned then running", statuses)
	}
}

func TestStartingAgainKeepsARunningServiceAndReplacesAStoppedOne(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	first := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]

	again := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]
	if !again.Running || again.ID != first.ID || again.HostPort != first.HostPort || again.Note != "already running" {
		t.Errorf("second start = %+v, want the same container kept", again)
	}

	dk.Exit(first.ID, 1)
	third := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]
	if !third.Running || third.ID == first.ID || third.HostPort != first.HostPort {
		t.Errorf("after the container stopped: %+v; want a new container on the same port", third)
	}
	if list, _ := dk.ListContainers(ctx, map[string]string{"rig.ticket": "demo"}); len(list) != 1 {
		t.Errorf("containers = %d, want one", len(list))
	}
}

func TestAMissingDockerfileFailsBeforeAnythingIsCreated(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, false, apiService) // the repo has no Dockerfile
	_, err := f.start("demo", domain.SandboxContainer)
	if apiCode(t, err) != api.CodeInvalid || !strings.Contains(err.Error(), "Dockerfile") {
		t.Fatalf("error = %v", err)
	}
	if nets, _ := dk.ListNetworks(ctx, nil); len(nets) != 0 {
		t.Errorf("networks created before the check: %v", nets)
	}
	if list, _ := dk.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers created: %v", list)
	}
	if ok, _ := dk.ImageExists(ctx, "rig-demo-svc1:latest"); ok {
		t.Error("an image was built")
	}
	if f.state("demo").Services["svc1"].HostPort != 0 {
		t.Error("a port was allocated")
	}
}

func TestBadServiceSettingsAreRefusedUpFront(t *testing.T) {
	tests := []struct {
		name, extra, want string
	}{
		{"a healthcheck that is not a path", "build: {dockerfile: Dockerfile}\n      internal_port: 80\n      healthcheck: healthz", "starting with /"},
		{"a healthcheck without a port", "build: {dockerfile: Dockerfile}\n      healthcheck: /healthz", "internal_port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, dk, _ := serviceFixture(t, nil)
			repo := testkit.NewRepo(t)
			withService(t, f.create("demo", repo), repo, true, tt.extra)
			_, err := f.start("demo", domain.SandboxContainer)
			if apiCode(t, err) != api.CodeInvalid || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want invalid mentioning %q", err, tt.want)
			}
			if nets, _ := dk.ListNetworks(ctx, nil); len(nets) != 0 {
				t.Error("something was created")
			}
		})
	}
}

func TestAServiceWithAStartCommandButNoDockerfileIsReported(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, false, `start: "bun dev"`)
	res := f.mustStart("demo", domain.SandboxContainer)
	c := containerByService(res)["svc1"]
	if c.Running || c.Error != "" || !strings.Contains(c.Note, "build.dockerfile") {
		t.Errorf("container = %+v, want a note, not a failure", c)
	}
	if list, _ := dk.ListContainers(ctx, nil); len(list) != 0 {
		t.Error("a container was started")
	}
}

func TestAnUnhealthyServiceIsAnErrorWithItsLastLogLines(t *testing.T) {
	f, dk, probe := serviceFixture(t, nil)
	probe.set(false)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)

	// the container prints something before the check gives up
	go func() {
		for { // wait for the container to exist
			if _, err := dk.InspectContainer(ctx, "rig-demo-svc1"); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		for i := 0; i < 200; i++ {
			dk.Print("rig-demo-svc1", "booting step "+fmtInt(i))
		}
	}()
	res := f.mustStart("demo", domain.SandboxContainer)
	c := containerByService(res)["svc1"]
	if c.Running || !strings.Contains(c.Error, "not healthy after") || !strings.Contains(c.Error, "/healthz") || !strings.Contains(c.Error, "last log lines") {
		t.Fatalf("container = %+v", c)
	}
	if strings.Count(c.Error, "booting step") > 20 {
		t.Errorf("the error carries more than 20 log lines:\n%s", c.Error)
	}
	st := f.state("demo").Services["svc1"]
	if st.Status != domain.ServiceError || !strings.Contains(st.Error, "not healthy") {
		t.Errorf("state = %+v", st)
	}
}

func TestAServiceThatExitsDuringStartupIsAnError(t *testing.T) {
	f, dk, probe := serviceFixture(t, func(o *lifecycle.Options) { o.ServiceStartTimeout = 5 * time.Second })
	probe.set(false)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	go func() {
		for i := 0; i < 500; i++ {
			if _, err := dk.InspectContainer(ctx, "rig-demo-svc1"); err == nil {
				dk.Print("rig-demo-svc1", "Error: cannot find module './server.js'")
				dk.Exit("rig-demo-svc1", 1)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	c := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]
	if c.Running || !strings.Contains(c.Error, "exited with code 1") || !strings.Contains(c.Error, "cannot find module") {
		t.Errorf("container = %+v", c)
	}
}

func TestHealthFlipsAreReflectedInTheStatus(t *testing.T) {
	f, _, probe := serviceFixture(t, func(o *lifecycle.Options) { o.ServiceStartTimeout = 5 * time.Second })
	probe.set(false)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	go func() { time.Sleep(50 * time.Millisecond); probe.set(true) }()
	c := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]
	if !c.Running || c.Error != "" || f.state("demo").Services["svc1"].Status != domain.ServiceRunning {
		t.Errorf("a service that became healthy later: %+v", c)
	}
}

func TestAServiceWithoutAHealthcheckNeedsToStayUp(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, "build: {dockerfile: Dockerfile}\n      start: \"./run\"")
	c := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]
	if !c.Running || c.HostPort != 0 || c.URL != "" {
		t.Errorf("container = %+v, want running with no port", c)
	}
	if got, _ := dk.InspectContainer(ctx, "rig-demo-svc1"); len(got.Ports) != 0 {
		t.Errorf("ports = %+v", got.Ports)
	}
}

func TestOneFailingServiceDoesNotStopTheOthers(t *testing.T) {
	f, dk, probe := serviceFixture(t, nil)
	r1, r2 := testkit.NewRepo(t), testkit.NewRepo(t)
	path := f.create("demo", r1, r2)
	r1.Commit("Dockerfile", "FROM scratch\n", "df")
	r2.Commit("Dockerfile", "FROM scratch\n", "df")
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	patched := strings.Replace(string(raw), "      branch: rig/demo/svc1", "      build: {dockerfile: Dockerfile}\n      internal_port: 8080\n      healthcheck: /ok\n      branch: rig/demo/svc1", 1)
	patched = strings.Replace(patched, "      branch: rig/demo/svc2", "      build: {dockerfile: Dockerfile}\n      internal_port: 9090\n      healthcheck: /ok\n      branch: rig/demo/svc2", 1)
	os.WriteFile(filepath.Join(path, "ticket.yaml"), []byte(patched), 0o600)
	dk.Down(nil)
	probe.set(true)

	res := f.mustStart("demo", domain.SandboxContainer)
	by := containerByService(res)
	if !by["svc1"].Running || !by["svc2"].Running || by["svc1"].HostPort == by["svc2"].HostPort {
		t.Errorf("containers = %+v", by)
	}
}

func TestSecretsReachTheServiceEnvironmentOnly(t *testing.T) {
	f, dk, _ := serviceFixture(t, func(o *lifecycle.Options) {
		o.Secrets = func(names []string) (map[string]string, error) {
			return map[string]string{"DB_PASSWORD": "sentinel-pw"}, nil
		}
	})
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, apiService)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("secrets: [DB_PASSWORD]\n")...), 0o600)
	res := f.mustStart("demo", domain.SandboxContainer)
	got, _ := dk.InspectContainer(ctx, "rig-demo-svc1")
	if got.Env["DB_PASSWORD"] != "sentinel-pw" {
		t.Errorf("env = %v", got.Env)
	}
	state, _ := os.ReadFile(filepath.Join(path, "state.json"))
	if strings.Contains(string(state), "sentinel-pw") || strings.Contains(res.Containers[0].Error, "sentinel-pw") {
		t.Error("the secret value leaked into state or the result")
	}

	g, _, _ := serviceFixture(t, nil) // no resolver
	repo2 := testkit.NewRepo(t)
	path2 := g.create("other", repo2)
	withService(t, path2, repo2, true, apiService)
	raw2, _ := os.ReadFile(filepath.Join(path2, "ticket.yaml"))
	os.WriteFile(filepath.Join(path2, "ticket.yaml"), append(raw2, []byte("secrets: [DB_PASSWORD]\n")...), 0o600)
	c := containerByService(g.mustStart("other", domain.SandboxContainer))["svc1"]
	if c.Running || !strings.Contains(c.Error, "DB_PASSWORD") {
		t.Errorf("container = %+v, want an error naming the secret", c)
	}
}

func TestStopRemovesTheServiceContainerAndFreesItsPort(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	started := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]

	f.mustStop("demo", false, false)
	if list, _ := dk.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers left: %v", list)
	}
	if nets, _ := dk.ListNetworks(ctx, nil); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
	st := f.state("demo").Services["svc1"]
	if st.ContainerID != "" || st.HostPort != 0 || st.Status != domain.ServiceStopped {
		t.Errorf("state = %+v", st)
	}
	if len(dk.HostPorts()) != 0 {
		t.Errorf("host ports still held: %v", dk.HostPorts())
	}
	_ = started
}

func TestAServicesPortSurvivesADaemonRestart(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	first := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]

	// a new daemon: same state on disk, container still running and holding its port
	restarted := lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Docker: dk, HealthProbe: func(context.Context, string) bool { return true },
		Global:              config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, BranchPrefix: "rig/"}},
		ServiceStartTimeout: 300 * time.Millisecond, HealthPoll: time.Millisecond, Settle: 20 * time.Millisecond,
	})
	dk.Exit(first.ID, 0)
	again, err := restarted.Start(ctx, api.TicketStartParams{ID: "demo", Sandbox: domain.SandboxContainer})
	if err != nil {
		t.Fatal(err)
	}
	if c := containerByService(again)["svc1"]; !c.Running || c.HostPort != first.HostPort {
		t.Errorf("after a restart: %+v, want port %d kept", c, first.HostPort)
	}
}
