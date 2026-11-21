package lifecycle_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func trustedIn(t *testing.T, configDir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		HasCompletedOnboarding bool                      `json:"hasCompletedOnboarding"`
		Projects               map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || !c.HasCompletedOnboarding {
		t.Fatalf("config = %s, %v; onboarding must be done", raw, err)
	}
	var out []string
	for path, p := range c.Projects {
		if p["hasTrustDialogAccepted"] == true {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

func withAuth(profile config.AuthProfile) func(*lifecycle.Options) {
	return func(o *lifecycle.Options) {
		o.Global.Auth = config.Auth{Profiles: map[string]config.AuthProfile{"work": profile}}
		o.Secrets = func(names []string) (map[string]string, error) {
			out := map[string]string{}
			for _, n := range names {
				out[n] = "secret-" + n
			}
			return out, nil
		}
	}
}

func useAuth(t *testing.T, ticketPath string) {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(ticketPath, "ticket.yaml"))
	if err := os.WriteFile(filepath.Join(ticketPath, "ticket.yaml"), append(raw, []byte("auth: work\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// homeWatch points HOME at an empty directory and fails if anything appears in it.
func homeWatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() {
		if entries, _ := os.ReadDir(home); len(entries) > 0 {
			t.Errorf("rig wrote into the user's HOME: %v", entries)
		}
	})
}

func TestAHostAgentWithACredentialProfileGetsItsOwnTrustedConfig(t *testing.T) {
	homeWatch(t)
	f, term := agentFixture(t, withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}))
	path := f.create("demo", testkit.NewRepo(t))
	useAuth(t, path)
	res := f.mustStart("demo", domain.SandboxHost)

	sp := term.Spawned()[0]
	configDir := filepath.Join(path, ".rig-claude", "svc1")
	if sp.Env["CLAUDE_CONFIG_DIR"] != configDir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %s", sp.Env["CLAUDE_CONFIG_DIR"], configDir)
	}
	want := []string{resolveDir(res.Checkouts[0].Path), resolveDir(path)}
	slices.Sort(want)
	if got := trustedIn(t, configDir); !slices.Equal(got, want) {
		t.Errorf("trusted = %v, want exactly the real checkout and ticket paths %v", got, want)
	}
}

func TestAHostLoginAgentKeepsTheUsersOwnConfig(t *testing.T) {
	f, term := agentFixture(t, withAuth(config.AuthProfile{Type: domain.AuthHostLogin}))
	path := f.create("demo", testkit.NewRepo(t))
	useAuth(t, path)
	f.mustStart("demo", domain.SandboxHost)

	if _, ok := term.Spawned()[0].Env["CLAUDE_CONFIG_DIR"]; ok {
		t.Error("a host_login agent must keep the user's own configuration: that is where the login is")
	}
	if _, err := os.Stat(filepath.Join(path, ".rig-claude")); !os.IsNotExist(err) {
		t.Errorf("a config directory was made for a host_login agent: %v", err)
	}
}

func TestAContainerAgentGetsItsOwnTrustedConfig(t *testing.T) {
	homeWatch(t)
	f, dk, term := agentsFixture(t, withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}))
	path := f.create("demo", testkit.NewRepo(t))
	useAuth(t, path)
	if res := f.mustStart("demo", domain.SandboxContainer); res.Agents[0].Error != "" {
		t.Fatal(res.Agents[0].Error)
	}

	sp := term.Spawned()[0]
	if sp.Env["CLAUDE_CONFIG_DIR"] != "/rig/claude" || !strings.Contains(strings.Join(sp.Command, " "), "--env CLAUDE_CONFIG_DIR") {
		t.Errorf("env = %v, command = %.200q", sp.Env, sp.Command)
	}
	c, err := dk.InspectContainer(ctx, "rig-demo-svc1-agent")
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(path, ".rig-claude", "svc1")
	if !slices.Contains(mountSet(c), configDir+" -> /rig/claude rw") {
		t.Errorf("mounts = %v", mountSet(c))
	}
	if got := trustedIn(t, configDir); !slices.Equal(got, []string{"/rig/ticket", "/work"}) {
		t.Errorf("trusted = %v, want exactly the paths the agent sees", got)
	}
}

// Claude Code keys trust by the real path; a tickets directory reached through a
// symlink (as /tmp and /var are on macOS) must still be trusted where Claude looks.
func TestTrustIsForTheRealPathsWhenTheTicketsDirectoryIsALink(t *testing.T) {
	var link string
	f, term := agentFixture(t, func(o *lifecycle.Options) {
		withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}})(o)
		link = filepath.Join(t.TempDir(), "tickets-link")
		if err := os.Symlink(o.Dir, link); err != nil {
			t.Fatal(err)
		}
		o.Dir = link
	})
	path := f.create("demo", testkit.NewRepo(t))
	useAuth(t, path)
	res := f.mustStart("demo", domain.SandboxHost)

	configDir := term.Spawned()[0].Env["CLAUDE_CONFIG_DIR"]
	want := []string{resolveDir(res.Checkouts[0].Path), resolveDir(path)}
	slices.Sort(want)
	if got := trustedIn(t, configDir); !slices.Equal(got, want) {
		t.Errorf("trusted = %v, want the real paths %v", got, want)
	}
}

// roles writes an agent configuration template with one subagent.
func roles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if files == nil {
		files = map[string]string{"agents/reviewer.md": "review carefully"}
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func useAgentConfig(t *testing.T, ticketPath string, dirs ...string) {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(ticketPath, "ticket.yaml"))
	line := "agent_config: [" + strings.Join(dirs, ", ") + "]\n"
	if err := os.WriteFile(filepath.Join(ticketPath, "ticket.yaml"), append(raw, []byte(line)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAContainerAgentGetsItsAgentConfigReadOnly(t *testing.T) {
	f, dk, _ := agentsFixture(t, withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}))
	path := f.create("demo", testkit.NewRepo(t))
	useAuth(t, path)
	useAgentConfig(t, path, roles(t, nil))
	if res := f.mustStart("demo", domain.SandboxContainer); res.Agents[0].Error != "" {
		t.Fatal(res.Agents[0].Error)
	}

	c, err := dk.InspectContainer(ctx, "rig-demo-svc1-agent")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(path, ".rig-agent-config", "svc1")
	if !slices.Contains(mountSet(c), snapshot+" -> /rig/agent-config ro") {
		t.Errorf("mounts = %v; the snapshot must be mounted read-only", mountSet(c))
	}
	for _, m := range c.Mounts {
		if strings.HasPrefix(m.Target, "/work") && m.Target != "/work" {
			t.Errorf("%s is mounted inside the checkout", m.Target)
		}
	}
	link, err := os.Readlink(filepath.Join(path, ".rig-claude", "svc1", "agents"))
	if err != nil || link != "/rig/agent-config/agents" {
		t.Errorf("agents -> %q, %v", link, err)
	}
	if got, _ := os.ReadFile(filepath.Join(snapshot, "agents", "reviewer.md")); string(got) != "review carefully" {
		t.Errorf("snapshot reviewer.md = %q", got)
	}
}

func TestAHostAgentGetsItsAgentConfig(t *testing.T) {
	homeWatch(t)
	f, term := agentFixture(t, withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}))
	path := f.create("demo", testkit.NewRepo(t))
	useAuth(t, path)
	useAgentConfig(t, path, roles(t, nil))
	f.mustStart("demo", domain.SandboxHost)

	configDir := term.Spawned()[0].Env["CLAUDE_CONFIG_DIR"]
	if got, err := os.ReadFile(filepath.Join(configDir, "agents", "reviewer.md")); err != nil || string(got) != "review carefully" {
		t.Errorf("reviewer.md through the agent's config = %q, %v", got, err)
	}
}

func TestBadAgentConfigIsRefusedBeforeAnythingStarts(t *testing.T) {
	cases := []struct {
		name    string
		sandbox domain.Sandbox
		profile config.AuthProfile
		dir     func(t *testing.T) string
		want    string
	}{
		{"missing", domain.SandboxContainer, config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}},
			func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") }, "does not exist"},
		{"a known secret", domain.SandboxContainer, config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}},
			func(t *testing.T) string { return roles(t, map[string]string{"CLAUDE.md": "token secret-WORK_TOKEN"}) }, "WORK_TOKEN"},
		{"the user's own config", domain.SandboxHost, config.AuthProfile{Type: domain.AuthHostLogin},
			func(t *testing.T) string { return roles(t, nil) }, "own configuration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, dk, term := agentsFixture(t, withAuth(c.profile))
			path := f.create("demo", testkit.NewRepo(t))
			useAuth(t, path)
			useAgentConfig(t, path, c.dir(t))

			_, err := f.start("demo", c.sandbox)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
			if _, err := os.Stat(filepath.Join(path, "svc1")); !os.IsNotExist(err) {
				t.Error("a checkout was made")
			}
			if _, err := dk.InspectContainer(ctx, "rig-demo-svc1-agent"); err == nil {
				t.Error("an agent container was made")
			}
			if len(term.Spawned()) > 0 {
				t.Error("an agent was started")
			}
		})
	}
}

func TestEachServiceGetsItsOwnAgentConfigAndModel(t *testing.T) {
	f, _, term := agentsFixture(t, withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}))
	path := f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	agree(t, path)
	useAuth(t, path)
	ticketWide := roles(t, map[string]string{"CLAUDE.md": "ticket rules"})
	own := roles(t, map[string]string{"CLAUDE.md": "svc2 rules"})
	useAgentConfig(t, path, ticketWide)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	withService := strings.Replace(string(raw), "- name: svc2\n", "- name: svc2\n      agent_config: ["+own+"]\n      model: claude-sonnet-5-5\n", 1)
	if withService == string(raw) {
		t.Fatalf("could not find svc2 in:\n%s", raw)
	}
	os.WriteFile(filepath.Join(path, "ticket.yaml"), []byte(withService), 0o600)

	res := f.mustStart("demo", domain.SandboxContainer)
	for _, a := range res.Agents {
		if a.Error != "" {
			t.Fatal(a.Error)
		}
	}
	for svc, want := range map[string]string{"svc1": "ticket rules", "svc2": "svc2 rules"} {
		if got, _ := os.ReadFile(filepath.Join(path, ".rig-agent-config", svc, "CLAUDE.md")); string(got) != want {
			t.Errorf("%s CLAUDE.md = %q, want %q", svc, got, want)
		}
	}
	for _, s := range term.Spawned() {
		has := slices.Contains(s.Command, "claude-sonnet-5-5")
		if svc2 := s.Env["RIG_SERVICE"] == "svc2" || strings.Contains(strings.Join(s.Command, " "), "rig-demo-svc2-agent"); svc2 != has {
			t.Errorf("model on %v: %v", s.Command[:6], has)
		}
	}
}
