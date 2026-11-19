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
