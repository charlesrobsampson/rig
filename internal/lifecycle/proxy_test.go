package lifecycle_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func internalFixture(t *testing.T, mutate func(*lifecycle.Options)) (*fixture, *testkit.FakeDocker, *testkit.FakeTerminal) {
	t.Helper()
	return agentsFixture(t, func(o *lifecycle.Options) {
		o.Global.Defaults.NetworkMode = domain.NetworkInternal
		o.Global.Egress = config_Egress()
		if mutate != nil {
			mutate(o)
		}
	})
}

func TestInternalModeRunsAProxyThatIsTheOnlyThingOnTheEgressNetwork(t *testing.T) {
	f, dk, _ := internalFixture(t, nil)
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, "build: {dockerfile: Dockerfile}\n      internal_port: 8080\n      egress: {allow: [registry.npmjs.org]}")
	addDeps(t, path, "dependencies:\n  - name: db\n    image: postgres:16\negress: {allow: [\"*.example.org\", API.anthropic.com]}\n")
	dk.AddImage("postgres:16")
	res := f.mustStart("demo", domain.SandboxContainer)
	if res.Network == nil || !res.Network.Internal || res.Network.Egress != "rig-demo-egress" {
		t.Fatalf("network = %+v", res.Network)
	}

	proxy, err := dk.InspectContainer(ctx, "rig-demo-proxy")
	if err != nil || proxy.State != docker.StateRunning {
		t.Fatalf("proxy = %+v, %v", proxy, err)
	}
	if proxy.Labels["rig.role"] != "proxy" || proxy.Labels["rig.ticket"] != "demo" {
		t.Errorf("labels = %v", proxy.Labels)
	}
	if got := proxy.Env["ALLOW_HOSTS"]; got != "*.example.org,api.anthropic.com,registry.npmjs.org" {
		t.Errorf("allowlist = %q, want the global default, the ticket's and the service's, lowercased, sorted", got)
	}
	if proxy.Env["ALLOW_PORTS"] != "443" || proxy.Env["LISTEN"] != ":3128" {
		t.Errorf("env = %v", proxy.Env)
	}
	if _, onEgress := proxy.Networks["rig-demo-egress"]; !onEgress {
		t.Errorf("the proxy needs the egress network to reach out: %v", proxy.Networks)
	}
	if aliases := proxy.Networks["rig-demo"].Aliases; !contains(aliases, "rig-demo-proxy") {
		t.Errorf("the proxy must be reachable by name on the internal network: %v", proxy.Networks)
	}

	// everything else is on the internal network only
	egressNet, _ := dk.InspectNetwork(ctx, "rig-demo-egress")
	if len(egressNet.Containers) != 1 || egressNet.Containers[0] != proxy.ID {
		t.Errorf("egress network members = %v, want only the proxy %s", egressNet.Containers, proxy.ID)
	}
	for _, name := range []string{"rig-demo-svc1", "rig-demo-svc1-agent", "rig-demo-dep-db"} {
		c, err := dk.InspectContainer(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, leaked := c.Networks["rig-demo-egress"]; leaked || len(c.Networks) != 1 {
			t.Errorf("%s is on %v; it must not touch the egress network", name, c.Networks)
		}
	}
}

func TestContainersGetTheProxySettings(t *testing.T) {
	f, dk, term := internalFixture(t, nil)
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, "build: {dockerfile: Dockerfile}\n      internal_port: 8080")
	addDeps(t, path, "dependencies:\n  - name: db\n    image: postgres:16\n")
	dk.AddImage("postgres:16")
	f.mustStart("demo", domain.SandboxContainer)

	svc, _ := dk.InspectContainer(ctx, "rig-demo-svc1")
	db, _ := dk.InspectContainer(ctx, "rig-demo-dep-db")
	pane := term.Spawned()[0]
	proxyURL := "http://rig-demo-proxy:3128"
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if svc.Env[k] != proxyURL || pane.Env[k] != proxyURL {
			t.Errorf("%s: service %q, agent %q, want %s", k, svc.Env[k], pane.Env[k], proxyURL)
		}
		if !strings.Contains(strings.Join(pane.Command, " "), "--env "+k) {
			t.Errorf("the agent's docker exec does not pass %s", k)
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		for _, want := range []string{"localhost", "127.0.0.1", "rig-demo-proxy", "svc1", "db", "rig-demo-svc1", "rig-demo-dep-db"} {
			if !contains(strings.Split(svc.Env[k], ","), want) || !contains(strings.Split(pane.Env[k], ","), want) {
				t.Errorf("%s lacks %q: service %q", k, want, svc.Env[k])
			}
		}
	}
	if _, has := db.Env["HTTPS_PROXY"]; has {
		t.Error("a dependency has no use for the proxy and should not be pointed at it")
	}
	if pane.Env["DISABLE_TELEMETRY"] != "1" {
		t.Error("an internal-mode agent should not try to reach telemetry hosts")
	}
}

func TestBridgeModeHasNoProxy(t *testing.T) {
	f, dk, term := agentsFixture(t, nil) // bridge by default
	f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxContainer)
	if _, err := dk.InspectContainer(ctx, "rig-demo-proxy"); err == nil {
		t.Error("a proxy was started in bridge mode")
	}
	if _, has := term.Spawned()[0].Env["HTTPS_PROXY"]; has {
		t.Error("bridge mode must not point agents at a proxy")
	}
	if ok, _ := dk.ImageExists(ctx, "rig-egress:"+"anything"); ok {
		t.Error("an egress image was built in bridge mode")
	}
	for _, b := range dk.Builds() {
		if b.Labels["rig.egress-image"] != "" {
			t.Errorf("the egress image was built in bridge mode: %+v", b)
		}
	}
}

func TestTheProxyImageIsBuiltOnceAcrossTickets(t *testing.T) {
	f, dk, _ := internalFixture(t, nil)
	f.create("one", testkit.NewRepo(t))
	f.create("two", testkit.NewRepo(t))
	f.mustStart("one", domain.SandboxContainer)
	f.mustStart("two", domain.SandboxContainer)
	n := 0
	for _, b := range dk.Builds() {
		if b.Labels["rig.egress-image"] != "" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d proxy image builds, want 1", n)
	}
	if _, err := dk.InspectContainer(ctx, "rig-one-proxy"); err != nil {
		t.Error("ticket one has no proxy")
	}
	if _, err := dk.InspectContainer(ctx, "rig-two-proxy"); err != nil {
		t.Error("ticket two has no proxy")
	}
}

func TestAProxyThatCannotRunFailsTheStartBeforeAnyContainer(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*lifecycle.Options)
		want   string
	}{
		{"the proxy binary is not in this build", func(o *lifecycle.Options) {
			o.EgressBinary = func(string) ([]byte, error) { return nil, errors.New("this rig was built without the egress proxy") }
		}, "built without the egress proxy"},
		{"an invalid allowlist entry", func(o *lifecycle.Options) { o.Global.Egress.DefaultAllow = []string{"bad host!"} }, "egress allowlist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, dk, term := internalFixture(t, tt.mutate)
			f.create("demo", testkit.NewRepo(t))
			_, err := f.start("demo", domain.SandboxContainer)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if list, _ := dk.ListContainers(ctx, map[string]string{"rig.ticket": "demo"}); len(list) != 0 {
				t.Errorf("containers started without containment: %v", list)
			}
			if len(term.Spawned()) != 0 {
				t.Error("an agent was started without a proxy")
			}
		})
	}
}

func TestChangingTheAllowlistReplacesTheRunningProxy(t *testing.T) {
	f, dk, _ := internalFixture(t, nil)
	path := f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxContainer)
	first, _ := dk.InspectContainer(ctx, "rig-demo-proxy")

	f.mustStart("demo", domain.SandboxContainer)
	same, _ := dk.InspectContainer(ctx, "rig-demo-proxy")
	if same.ID != first.ID {
		t.Error("a start with an unchanged allowlist replaced the proxy")
	}

	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("egress: {allow: [pypi.org]}\n")...), 0o600)
	f.mustStart("demo", domain.SandboxContainer)
	changed, err := dk.InspectContainer(ctx, "rig-demo-proxy")
	if err != nil || changed.ID == first.ID || !strings.Contains(changed.Env["ALLOW_HOSTS"], "pypi.org") {
		t.Errorf("proxy after the edit = %+v, %v; want a new one that allows pypi.org", changed, err)
	}
}

func TestStopRemovesTheProxyAndBothNetworks(t *testing.T) {
	f, dk, _ := internalFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxContainer)
	f.mustStop("demo", false, false)
	if list, _ := dk.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers left: %v", list)
	}
	if nets, _ := dk.ListNetworks(ctx, nil); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
	if st := f.state("demo"); st.EgressNetwork != nil {
		t.Errorf("state still records the egress network: %+v", st.EgressNetwork)
	}
}

func TestEgressDeniedIsDeclaredAsAnEventTheProxyLogFeeds(t *testing.T) {
	// the daemon-side watcher is tested in internal/egress; here only that the
	// lifecycle labels the proxy so the watcher can find it and its ticket
	f, dk, _ := internalFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxContainer)
	list, _ := dk.ListContainers(ctx, map[string]string{docker.LabelRole: docker.RoleProxy})
	if len(list) != 1 || list[0].Labels[docker.LabelTicket] != "demo" {
		t.Errorf("proxies by label = %+v", list)
	}
	_ = api.EventEgressDenied
}

func config_Egress() config.Egress {
	return config.Egress{DefaultAllow: []string{"api.anthropic.com"}, DefaultPorts: []int{443}}
}

func TestInternalModePublishesServicePortsThroughTheProxy(t *testing.T) {
	f, dk, _ := internalFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	res := f.mustStart("demo", domain.SandboxContainer)
	c := containerByService(res)["svc1"]
	if !c.Running || c.HostPort < 41000 || c.URL == "" {
		t.Fatalf("service = %+v", c)
	}

	svc, _ := dk.InspectContainer(ctx, "rig-demo-svc1")
	if len(svc.Ports) != 0 {
		t.Errorf("the service publishes %v itself; on an internal network Docker cannot, so the proxy does", svc.Ports)
	}
	proxy, _ := dk.InspectContainer(ctx, "rig-demo-proxy")
	if len(proxy.Ports) != 1 || proxy.Ports[0].Host != c.HostPort || proxy.Ports[0].Container != c.HostPort {
		t.Errorf("proxy ports = %+v, want the service's host port %d carried inward", proxy.Ports, c.HostPort)
	}
	if want := "FORWARD"; proxy.Env[want] != itoa(c.HostPort)+"=svc1:8080" {
		t.Errorf("FORWARD = %q, want %d=svc1:8080", proxy.Env[want], c.HostPort)
	}
	if got := f.state("demo").Services["svc1"].HostPort; got != c.HostPort {
		t.Errorf("state host port = %d, want %d", got, c.HostPort)
	}

	// a repeated start keeps both
	again := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]
	proxy2, _ := dk.InspectContainer(ctx, "rig-demo-proxy")
	if again.HostPort != c.HostPort || proxy2.ID != proxy.ID {
		t.Errorf("second start: port %d (was %d), proxy %s (was %s)", again.HostPort, c.HostPort, proxy2.ID, proxy.ID)
	}
}

func TestBridgeModeStillPublishesOnTheServiceItself(t *testing.T) {
	f, dk, _ := agentsFixture(t, nil)
	repo := testkit.NewRepo(t)
	withService(t, f.create("demo", repo), repo, true, apiService)
	c := containerByService(f.mustStart("demo", domain.SandboxContainer))["svc1"]
	svc, _ := dk.InspectContainer(ctx, "rig-demo-svc1")
	if len(svc.Ports) != 1 || svc.Ports[0].Host != c.HostPort {
		t.Errorf("ports = %+v", svc.Ports)
	}
}
