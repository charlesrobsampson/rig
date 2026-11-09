package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// agentsFixture runs agents in containers: a fake Docker and a fake terminal.
func agentsFixture(t *testing.T, mutate func(*lifecycle.Options)) (*fixture, *testkit.FakeDocker, *testkit.FakeTerminal) {
	t.Helper()
	f := newFixture(t, domain.SandboxContainer)
	dk, term := testkit.NewFakeDocker(), testkit.NewFakeTerminal()
	opts := lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Docker: dk, Backend: term, BackendName: "fake",
		HealthProbe: func(context.Context, string) bool { return true },
		Global: config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, BranchPrefix: "rig/"},
			Docker: config.Docker{AgentBase: "debian:bookworm-slim", ClaudeVersion: "latest"}},
		ServiceStartTimeout: 300 * time.Millisecond, HealthPoll: time.Millisecond, Settle: 20 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	f.lc = lifecycle.New(opts)
	return f, dk, term
}

func mountSet(c docker.ContainerInfo) []string {
	var out []string
	for _, m := range c.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		out = append(out, m.Source+" -> "+m.Target+" "+mode)
	}
	sort.Strings(out)
	return out
}

func TestTheAgentContainerHoldsOnlyWhatTheAgentNeeds(t *testing.T) {
	f, dk, _ := agentsFixture(t, nil)
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	res := f.mustStart("demo", domain.SandboxContainer)
	a := res.Agents[0]
	if !a.Launched || a.Container != "rig-demo-svc1-agent" || a.Error != "" {
		t.Fatalf("agent = %+v", a)
	}
	c, err := dk.InspectContainer(ctx, "rig-demo-svc1-agent")
	if err != nil {
		t.Fatal(err)
	}

	checkout := res.Checkouts[0].Path
	objects := resolveDir(filepath.Join(repo.Dir, ".git", "objects"))
	want := []string{
		checkout + " -> /work rw",
		objects + " -> " + objects + " ro",
		filepath.Join(path, "plan.md") + " -> /rig/ticket/plan.md rw",
		filepath.Join(path, "tasks.md") + " -> /rig/ticket/tasks.md rw",
		filepath.Join(path, "manual-tests.md") + " -> /rig/ticket/manual-tests.md rw",
		filepath.Join(path, ".rig-events", "svc1") + " -> /rig-events rw",
	}
	sort.Strings(want)
	if got := mountSet(c); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("mounts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, m := range c.Mounts {
		for _, forbidden := range []string{repo.Dir, filepath.Join(repo.Dir, ".git"), filepath.Join(path, "state.json"), filepath.Join(path, "ticket.yaml")} {
			if m.Source == forbidden || strings.HasPrefix(m.Source, forbidden+"/hooks") || strings.HasSuffix(m.Source, "/.git/config") || strings.HasSuffix(m.Source, "/.git/hooks") {
				t.Errorf("the container mounts %s", m.Source)
			}
		}
	}
	if c.Labels["rig.role"] != "agent" || c.Labels["rig.service"] != "svc1" || c.Labels["rig.ticket"] != "demo" {
		t.Errorf("labels = %v", c.Labels)
	}
	if _, onNet := c.Networks["rig-demo"]; !onNet || len(c.Networks) != 1 {
		t.Errorf("networks = %v, want only the ticket's", c.Networks)
	}
	spec := dk.ContainerSpec("rig-demo-svc1-agent")
	if spec.User != itoaID(os.Getuid())+":"+itoaID(os.Getgid()) || spec.WorkDir != "/work" || !spec.TTY || !spec.Stdin {
		t.Errorf("spec = %+v", spec)
	}
	if strings.Join(spec.Command, " ") != "sleep infinity" {
		t.Errorf("the container idles; the agent is exec'd into it: %v", spec.Command)
	}
	if spec.Image == "" || !strings.HasPrefix(spec.Image, "rig-agent:") {
		t.Errorf("image = %q", spec.Image)
	}
}

func itoaID(n int) string { return strconv.Itoa(n) }

func resolveDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func TestThePaneExecsIntoTheContainerAndSecretsStayOffTheCommandLine(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///custom/docker.sock")
	f, _, term := agentsFixture(t, func(o *lifecycle.Options) {
		o.Global.Auth = config.Auth{Profiles: map[string]config.AuthProfile{"work": {Type: domain.AuthAPIKey, Secrets: []string{"WORK_KEY"}}}}
		o.Secrets = func(names []string) (map[string]string, error) {
			return map[string]string{"WORK_KEY": "sk-sentinel"}, nil
		}
	})
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("auth: work\n")...), 0o600)
	res := f.mustStart("demo", domain.SandboxContainer)
	if res.Agents[0].Error != "" {
		t.Fatal(res.Agents[0].Error)
	}

	sp := term.Spawned()[0]
	head := strings.Join(sp.Command[:6], " ")
	if head != "docker exec -it --workdir /work" && !strings.HasPrefix(head, "docker exec -it --workdir /work") {
		t.Errorf("command starts %q", head)
	}
	cmd := strings.Join(sp.Command, " ")
	if !strings.Contains(cmd, "--env ANTHROPIC_API_KEY") || !strings.Contains(cmd, "--env RIG_SERVICE") || !strings.Contains(cmd, "rig-demo-svc1-agent claude --settings") || !strings.Contains(cmd, "--add-dir /rig/ticket") {
		t.Errorf("command = %.300s", cmd)
	}
	if strings.Contains(cmd, "sk-sentinel") {
		t.Error("the API key is on the command line")
	}
	if sp.Env["ANTHROPIC_API_KEY"] != "sk-sentinel" || sp.Env["RIG_TICKET"] != "demo" || sp.Env["DOCKER_HOST"] != "unix:///custom/docker.sock" {
		t.Errorf("pane env = %v", sp.Env)
	}
	if sp.Dir != res.Checkouts[0].Path {
		t.Errorf("pane dir = %s", sp.Dir)
	}
	if !strings.Contains(cmd, "/rig-events/events.log") {
		t.Error("the hooks must write to the events file the daemon watches")
	}
	if strings.Contains(cmd, "agent-event") {
		t.Error("a container has no rig binary to run")
	}
	st := f.state("demo").Services["svc1"].Agent
	if st.ContainerID == "" || st.PaneID == "" {
		t.Errorf("agent state = %+v", st)
	}
}

func TestTheContainerCarriesNoCredentialsFromTheHost(t *testing.T) {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "SSH_AUTH_SOCK", "GIT_ASKPASS", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "GIT_CONFIG_GLOBAL"} {
		t.Setenv(k, "host-"+k)
	}
	f, dk, term := agentsFixture(t, nil)
	repo := testkit.NewRepo(t)
	repo.Git("config", "user.name", "Ada Tester")
	repo.Git("config", "user.email", "ada@example.test")
	f.create("demo", repo)
	f.mustStart("demo", domain.SandboxContainer)

	c, _ := dk.InspectContainer(ctx, "rig-demo-svc1-agent")
	for k, v := range c.Env {
		if strings.Contains(v, "host-") {
			t.Errorf("container env %s=%s comes from the host", k, v)
		}
	}
	if c.Env["GIT_AUTHOR_NAME"] != "Ada Tester" || c.Env["GIT_COMMITTER_EMAIL"] != "ada@example.test" {
		t.Errorf("git identity = %v", c.Env)
	}
	sp := term.Spawned()[0]
	for k, v := range sp.Env {
		if strings.HasPrefix(v, "host-") {
			t.Errorf("pane env %s=%s: the host's credentials must not reach the agent unless listed in secrets", k, v)
		}
	}
	for _, m := range c.Mounts {
		if strings.Contains(m.Source, "/.ssh") || strings.Contains(m.Source, "docker.sock") || strings.HasSuffix(m.Target, "/.gitconfig") {
			t.Errorf("credentials mounted: %+v", m)
		}
	}
}

func TestAHostLoginProfileCannotWorkInAContainer(t *testing.T) {
	f, dk, term := agentsFixture(t, func(o *lifecycle.Options) {
		o.Global.Auth = config.Auth{Profiles: map[string]config.AuthProfile{"mine": {Type: domain.AuthHostLogin}}}
	})
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("auth: mine\n")...), 0o600)
	_, err := f.start("demo", domain.SandboxContainer)
	if apiCode(t, err) != api.CodeInvalid || !strings.Contains(err.Error(), "machine's own login") {
		t.Errorf("error = %v, want invalid before anything is created", err)
	}
	if exists(filepath.Join(f.dir, "demo", "svc1")) {
		t.Error("a checkout was created")
	}
	if _, err := dk.InspectContainer(ctx, "rig-demo-svc1-agent"); err == nil {
		t.Error("an agent container was created")
	}
	if len(term.Spawned()) != 0 {
		t.Error("a pane was opened")
	}
}

func TestTheAgentImageBuildsOnTheServicesOwnImageOrTheConfiguredBase(t *testing.T) {
	t.Run("a service with a Dockerfile", func(t *testing.T) {
		f, dk, _ := agentsFixture(t, nil)
		repo := testkit.NewRepo(t)
		withService(t, f.create("demo", repo), repo, true, apiService)
		f.mustStart("demo", domain.SandboxContainer)
		var bases []string
		for _, b := range dk.Builds() {
			if b.Args["BASE"] != "" {
				bases = append(bases, b.Args["BASE"])
			}
		}
		if len(bases) != 1 || bases[0] != "rig-demo-svc1:latest" {
			t.Errorf("agent image bases = %v, want the service's own image", bases)
		}
	})
	t.Run("a service without one", func(t *testing.T) {
		f, dk, _ := agentsFixture(t, func(o *lifecycle.Options) { o.Global.Docker.AgentBase = "alpine:3.22" })
		f.create("demo", testkit.NewRepo(t))
		f.mustStart("demo", domain.SandboxContainer)
		if b := dk.Builds(); len(b) != 1 || b[0].Args["BASE"] != "alpine:3.22" {
			t.Errorf("builds = %+v", b)
		}
	})
	t.Run("the image is built once for two tickets", func(t *testing.T) {
		f, dk, _ := agentsFixture(t, nil)
		f.create("one", testkit.NewRepo(t))
		f.create("two", testkit.NewRepo(t))
		f.mustStart("one", domain.SandboxContainer)
		f.mustStart("two", domain.SandboxContainer)
		if n := len(dk.Builds()); n != 1 {
			t.Errorf("%d agent image builds, want the cached image reused", n)
		}
	})
}

func TestHostModeKeepsTheOldPathWithALoudWarning(t *testing.T) {
	f, dk, term := agentsFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxHost)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "svc1") || !strings.Contains(res.Warnings[0], "directly on this machine") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	sp := term.Spawned()[0]
	if sp.Command[0] != "claude" || sp.Dir != res.Checkouts[0].Path {
		t.Errorf("host agent = %q in %s", sp.Command[:1], sp.Dir)
	}
	if list, _ := dk.ListContainers(ctx, map[string]string{"rig.role": "agent"}); len(list) != 0 {
		t.Errorf("a host-mode agent got a container: %v", list)
	}
}

func TestAContainerAgentSurvivesRepeatedStartsAndRelaunches(t *testing.T) {
	f, dk, term := agentsFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	first := f.mustStart("demo", domain.SandboxContainer).Agents[0]
	again := f.mustStart("demo", domain.SandboxContainer).Agents[0]
	if again.Launched || again.Note != "already running" || len(term.Spawned()) != 1 {
		t.Errorf("second start: %+v", again)
	}
	c1, _ := dk.InspectContainer(ctx, "rig-demo-svc1-agent")

	term.Exit(terminal.PaneID(first.PaneID), 0) // the agent quit; its container idles on
	third := f.mustStart("demo", domain.SandboxContainer).Agents[0]
	c2, _ := dk.InspectContainer(ctx, "rig-demo-svc1-agent")
	if !third.Launched || c2.ID != c1.ID {
		t.Errorf("relaunch: %+v, container %s -> %s; the running container should be reused", third, c1.ID, c2.ID)
	}
}

func TestStopRemovesTheAgentContainerBeforeTheCheckout(t *testing.T) {
	f, dk, term := agentsFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	f.mustStop("demo", false, false)
	if list, _ := dk.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers left: %v", list)
	}
	if panes, _ := term.List(ctx); len(panes) != 0 {
		t.Errorf("panes left: %v", panes)
	}
	if exists(res.Checkouts[0].Path) {
		t.Error("the checkout remains")
	}

	g, gdk, _ := agentsFixture(t, nil)
	g.create("demo", testkit.NewRepo(t))
	gres := g.mustStart("demo", domain.SandboxContainer)
	gdk.Down(docker.ErrUnavailable)
	if _, err := g.stop("demo", false, false); apiCode(t, err) != api.CodeUnavailable || !exists(gres.Checkouts[0].Path) {
		t.Errorf("a stop that cannot remove the agent container must keep the checkout: %v", err)
	}
}

func TestNoAgentIsStartedForAContainerServiceWithoutDocker(t *testing.T) {
	f, term := agentFixture(t, nil) // terminal only
	f.create("demo", testkit.NewRepo(t))
	a := f.mustStart("demo", domain.SandboxContainer).Agents[0]
	if a.Launched || !strings.Contains(a.Note, "needs Docker") || len(term.Spawned()) != 0 {
		t.Errorf("agent = %+v", a)
	}
}
