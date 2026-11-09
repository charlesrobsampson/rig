//go:build docker

package egress_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/dockercli"
	"github.com/charlesrobsampson/rig/internal/egress"
)

// The real containment and allow/deny behavior (spikes/egress/probe.sh as a test): an
// agent on an internal network reaches only what the proxy allows. It needs Docker, an
// image with curl (RIG_DOCKER_TEST_AGENT_IMAGE, default rig-agent-spike), network
// access to api.anthropic.com, and the embedded proxy binary (task build:egress).
func TestAnInternalNetworkContainerReachesOnlyWhatTheProxyAllows(t *testing.T) {
	dk := dockercli.New(dockercli.Options{})
	bg := context.Background()
	if err := dk.Ping(bg); err != nil {
		t.Skipf("Docker is not available: %v", err)
	}
	agentImage := os.Getenv("RIG_DOCKER_TEST_AGENT_IMAGE")
	if agentImage == "" {
		agentImage = "rig-agent-spike:latest"
	}
	if ok, _ := dk.ImageExists(bg, agentImage); !ok {
		t.Skipf("image %s (with curl) is not present locally", agentImage)
	}
	tag, _, err := egress.NewImages(dk, nil, "").Ensure(bg)
	if err != nil {
		t.Skipf("no proxy image: %v", err)
	}

	suffix := time.Now().UnixNano() % 1000000
	internal, external := "rigt-egress-int-"+itoa(suffix), "rigt-egress-ext-"+itoa(suffix)
	if _, err := dk.CreateNetwork(bg, docker.NetworkSpec{Name: internal, Internal: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dk.RemoveNetwork(bg, internal) })
	if _, err := dk.CreateNetwork(bg, docker.NetworkSpec{Name: external}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dk.RemoveNetwork(bg, external) })

	run := func(spec docker.ContainerSpec) {
		t.Helper()
		c, err := dk.CreateContainer(bg, spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { dk.RemoveContainer(bg, c.ID, true) })
		if err := dk.StartContainer(bg, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	proxy := "rigt-egress-proxy-" + itoa(suffix)
	run(docker.ContainerSpec{Name: proxy, Image: tag, Network: external,
		Env: map[string]string{"ALLOW_HOSTS": "api.anthropic.com,localhost", "ALLOW_PORTS": "443", "LISTEN": ":3128"}})
	if err := dk.ConnectNetwork(bg, proxy, internal, []string{"proxy"}); err != nil {
		t.Fatal(err)
	}
	service := "rigt-egress-svc-" + itoa(suffix)
	run(docker.ContainerSpec{Name: service, Image: agentImage, Network: internal, Command: []string{"node", "-e", "require('http').createServer((q,r)=>r.end('svc-ok')).listen(8080)"}})
	agent := "rigt-egress-agent-" + itoa(suffix)
	run(docker.ContainerSpec{Name: agent, Image: agentImage, Network: internal, Command: []string{"sleep", "300"}})
	time.Sleep(2 * time.Second)

	curl := func(env map[string]string, args ...string) string {
		t.Helper()
		res, err := dk.Exec(bg, agent, docker.ExecSpec{Env: env, Command: append([]string{"curl", "-s", "-m", "10", "-o", "/dev/null", "-w", "%{http_code}"}, args...)})
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(res.Output)
	}
	// curl only honors the lowercase http_proxy (docs/spikes/05-egress.md gotcha 1)
	viaProxy := map[string]string{"HTTPS_PROXY": "http://" + proxy + ":3128", "http_proxy": "http://" + proxy + ":3128"}

	t.Run("containment: direct access is impossible", func(t *testing.T) {
		if got := curl(nil, "https://example.com"); got != "000" {
			t.Errorf("direct https://example.com = %s, want 000", got)
		}
		if got := curl(nil, "http://1.1.1.1"); got != "000" {
			t.Errorf("direct raw IP = %s, want 000", got)
		}
		res, _ := dk.Exec(bg, agent, docker.ExecSpec{Command: []string{"getent", "hosts", "example.com"}})
		if res.ExitCode == 0 {
			t.Errorf("DNS for example.com answered: %s", res.Output)
		}
	})
	t.Run("through the proxy", func(t *testing.T) {
		if got := curl(viaProxy, "https://api.anthropic.com/"); got == "000" || got == "403" {
			t.Errorf("the allowed host = %s; the tunnel should work (any answer from the API)", got)
		}
		if got := curl(viaProxy, "https://example.com/"); got != "000" {
			t.Errorf("a denied host = %s, want 000 (CONNECT refused)", got)
		}
		if got := curl(viaProxy, "http://api.anthropic.com/"); got != "403" {
			t.Errorf("plain http = %s, want 403", got)
		}
		if got := curl(viaProxy, "https://api.anthropic.com:8443/"); got != "000" {
			t.Errorf("a port that is not allowed = %s, want 000", got)
		}
		if got := curl(viaProxy, "https://localhost/"); got != "000" {
			t.Errorf("an allowlisted name that resolves to loopback = %s, want 000", got)
		}
	})
	t.Run("service-to-service stays direct with NO_PROXY", func(t *testing.T) {
		env := map[string]string{"http_proxy": "http://" + proxy + ":3128", "no_proxy": service}
		if got := curl(env, "http://"+service+":8080/"); got != "200" {
			t.Errorf("direct service call = %s, want 200", got)
		}
		if got := curl(map[string]string{"http_proxy": "http://" + proxy + ":3128"}, "http://"+service+":8080/"); got == "200" {
			t.Error("without NO_PROXY the call should have gone through the proxy and been refused")
		}
	})
	t.Run("the proxy logged what it refused", func(t *testing.T) {
		r, err := dk.Logs(bg, proxy, docker.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		buf := make([]byte, 1<<16)
		n, _ := r.Read(buf)
		logs := string(buf[:n])
		for _, want := range []string{`"action":"deny"`, "example.com:443", "not in allowlist", "non-public address", "plain http not allowed", `"action":"allow"`} {
			if !strings.Contains(logs, want) {
				t.Errorf("proxy log lacks %q:\n%s", want, logs)
			}
		}
	})
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
