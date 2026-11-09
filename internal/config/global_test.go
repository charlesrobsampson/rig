package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
)

type env map[string]string

func (e env) get(k string) string { return e[k] }

// load writes doc as the config file (or none when doc is empty) and loads it.
func load(t *testing.T, doc string, e env) (config.Global, error) {
	t.Helper()
	home := t.TempDir()
	opts := config.GlobalOptions{Env: e.get, UserHome: func() (string, error) { return home, nil }}
	if doc != "" {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		opts.Path = path
	}
	return config.LoadGlobal(opts)
}

func TestLoadGlobalDefaultsWhenTheImplicitFileIsMissing(t *testing.T) {
	home := t.TempDir()
	got, err := config.LoadGlobal(config.GlobalOptions{
		Env:      env{}.get,
		UserHome: func() (string, error) { return home, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"harness", got.Defaults.Harness, "claude-code"},
		{"sandbox", got.Defaults.Sandbox, domain.SandboxContainer},
		{"terminal backend", got.Defaults.TerminalBackend, "herdr"},
		{"ticket source", got.Defaults.TicketSource, "manual"},
		{"network mode", got.Defaults.NetworkMode, domain.NetworkBridge},
		{"gates", got.Defaults.Policy.Gates, []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval}},
		{"push mode", got.Defaults.Policy.Push.Mode, domain.PushPR},
		{"max tickets", got.Limits.MaxActiveTickets, 8},
		{"branch prefix", got.Defaults.BranchPrefix, "rig/"},
		{"summarizer", got.Summarizer.Provider, "none"},
		{"egress allow", got.Egress.DefaultAllow, []string{"api.anthropic.com"}},
		{"egress ports", got.Egress.DefaultPorts, []int{443}},
		{"rig home", got.Paths.Home, filepath.Join(home, ".rig")},
		{"no auth profile by default", got.Auth.Default, ""},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadGlobalExplicitMissingFileIsAnError(t *testing.T) {
	_, err := config.LoadGlobal(config.GlobalOptions{Path: filepath.Join(t.TempDir(), "nope.json"), Env: env{}.get})
	if err == nil {
		t.Fatal("an explicitly requested config file that is missing must error")
	}
}

func TestLoadGlobalMergesAPartialFileOverDefaults(t *testing.T) {
	got, err := load(t, `{"limits":{"max_active_tickets":3},"defaults":{"sandbox":"host"}}`, env{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits.MaxActiveTickets != 3 || got.Defaults.Sandbox != domain.SandboxHost {
		t.Errorf("overrides not applied: %+v", got)
	}
	if got.Defaults.Harness != "claude-code" || got.Defaults.Policy.Push.Mode != domain.PushPR || len(got.Defaults.Policy.Gates) != 2 {
		t.Errorf("untouched defaults were lost: %+v", got.Defaults)
	}
}

func TestLoadGlobalFileCanReplaceOrEmptyTheDefaultGates(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []domain.Gate
	}{
		{"replaced", `{"defaults":{"policy":{"gates":["manual_test_signoff"]}}}`, []domain.Gate{domain.GateManualTestSignoff}},
		{"emptied", `{"defaults":{"policy":{"gates":[]}}}`, []domain.Gate{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := load(t, tt.doc, env{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Defaults.Policy.Gates, tt.want) {
				t.Errorf("gates = %#v, want %#v", got.Defaults.Policy.Gates, tt.want)
			}
		})
	}
}

func TestLoadGlobalRejectsBadConfigFiles(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{"unknown key", `{"defaults":{"harnes":"x"}}`, []string{"harnes"}},
		{"unknown top-level key", `{"bogus":1}`, []string{"bogus"}},
		{"bad sandbox", `{"defaults":{"sandbox":"vm"}}`, []string{"sandbox", "vm"}},
		{"bad terminal backend", `{"defaults":{"terminal_backend":"zellij"}}`, []string{"terminal_backend", "zellij", "herdr", "tmux"}},
		{"empty harness", `{"defaults":{"harness":""}}`, []string{"harness"}},
		{"bad summarizer provider", `{"summarizer":{"provider":"gpt"}}`, []string{"summarizer.provider", "gpt"}},
		{"bad branch prefix", `{"defaults":{"branch_prefix":"no slash"}}`, []string{"defaults.branch_prefix", "no slash"}},
		{"zero ticket cap", `{"limits":{"max_active_tickets":0}}`, []string{"max_active_tickets"}},
		{"syntax error reports a line", "{\n  \"limits\": {\n    \"max_active_tickets\": }\n}", []string{"line 3"}},
		{"wrong value type", `{"limits":{"max_active_tickets":"many"}}`, []string{"max_active_tickets"}},
		{"relative rig home", `{"paths":{"home":"rig-home"}}`, []string{"paths.home", "absolute"}},
		{"relative ca cert", `{"docker":{"extra_ca_certs":["certs/corp.pem"]}}`, []string{"docker.extra_ca_certs", "absolute"}},
		{"port range below 1024", `{"docker":{"port_range":{"min":80,"max":2000}}}`, []string{"docker.port_range", "80-2000"}},
		{"port range above 65535", `{"docker":{"port_range":{"min":41000,"max":70000}}}`, []string{"docker.port_range", "70000"}},
		{"port range backwards", `{"docker":{"port_range":{"min":42000,"max":41000}}}`, []string{"docker.port_range", "42000-41000"}},
		{"empty agent base", `{"docker":{"agent_base":""}}`, []string{"docker.agent_base"}},
		{"empty claude version", `{"docker":{"claude_version":""}}`, []string{"docker.claude_version"}},
		{"bad egress host", `{"egress":{"default_allow":["bad host!"]}}`, []string{"egress.default_allow", "bad host!"}},
		{"bad egress port", `{"egress":{"default_ports":[70000]}}`, []string{"egress.default_ports", "70000"}},
		{"several problems reported together", `{"limits":{"max_active_tickets":0},"defaults":{"harness":""}}`, []string{"max_active_tickets", "harness"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.doc, env{})
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestLoadGlobalExpandsTildeInPaths(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"paths":{"home":"~/rigwork"},"docker":{"extra_ca_certs":["~/certs/corp.pem"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadGlobal(config.GlobalOptions{Path: path, Env: env{}.get, UserHome: func() (string, error) { return home, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "rigwork"); got.Paths.Home != want {
		t.Errorf("home = %q, want %q", got.Paths.Home, want)
	}
	if want := filepath.Join(home, "certs", "corp.pem"); !reflect.DeepEqual(got.Docker.ExtraCACerts, []string{want}) {
		t.Errorf("extra_ca_certs = %v, want [%s]", got.Docker.ExtraCACerts, want)
	}
}

func TestLoadGlobalEnvironmentOverrides(t *testing.T) {
	t.Run("RIG_CONFIG selects the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(path, []byte(`{"limits":{"max_active_tickets":2}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := config.LoadGlobal(config.GlobalOptions{Env: env{"RIG_CONFIG": path}.get, UserHome: func() (string, error) { return t.TempDir(), nil }})
		if err != nil || got.Limits.MaxActiveTickets != 2 {
			t.Fatalf("got %+v, %v", got.Limits, err)
		}
	})
	t.Run("an explicit Path beats RIG_CONFIG", func(t *testing.T) {
		dir := t.TempDir()
		flagPath, envPath := filepath.Join(dir, "flag.json"), filepath.Join(dir, "env.json")
		os.WriteFile(flagPath, []byte(`{"limits":{"max_active_tickets":5}}`), 0o600)
		os.WriteFile(envPath, []byte(`{"limits":{"max_active_tickets":6}}`), 0o600)
		got, err := config.LoadGlobal(config.GlobalOptions{Path: flagPath, Env: env{"RIG_CONFIG": envPath}.get})
		if err != nil || got.Limits.MaxActiveTickets != 5 {
			t.Fatalf("got %+v, %v", got.Limits, err)
		}
	})
	t.Run("RIG_HOME beats the file and the default", func(t *testing.T) {
		home := t.TempDir()
		got, err := load(t, `{"paths":{"home":"/from/file"}}`, env{"RIG_HOME": home})
		if err != nil || got.Paths.Home != home {
			t.Fatalf("home = %q, %v, want %q", got.Paths.Home, err, home)
		}
	})
}

func TestLoadGlobalAuthProfiles(t *testing.T) {
	valid := `{"auth":{"default":"work","profiles":{
		"work":    {"type":"oauth_token","secrets":["CLAUDE_CODE_OAUTH_TOKEN_WORK"]},
		"api":     {"type":"api_key","secrets":["ANTHROPIC_API_KEY"]},
		"gateway": {"type":"auth_token","secrets":["GATEWAY_TOKEN"],"env":{"ANTHROPIC_BASE_URL":"https://gateway.internal"}},
		"cloud":   {"type":"bedrock","env":{"AWS_REGION":"us-east-1"}},
		"vertex":  {"type":"vertex"},
		"foundry": {"type":"foundry","secrets":["ANTHROPIC_FOUNDRY_API_KEY"]},
		"me":      {"type":"host_login"}
	}}}`
	got, err := load(t, valid, env{})
	if err != nil {
		t.Fatalf("valid profiles rejected: %v", err)
	}
	if got.Auth.Default != "work" || len(got.Auth.Profiles) != 7 || got.Auth.Profiles["gateway"].Type != domain.AuthAuthToken {
		t.Errorf("auth = %+v", got.Auth)
	}

	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{"default names a missing profile", `{"auth":{"default":"ghost","profiles":{}}}`, []string{"auth.default", "ghost"}},
		{"unknown type", `{"auth":{"profiles":{"p":{"type":"magic"}}}}`, []string{"magic"}},
		{"missing type", `{"auth":{"profiles":{"p":{"secrets":["A"]}}}}`, []string{"auth.profiles.p.type", "required"}},
		{"token profile without a secret name", `{"auth":{"profiles":{"p":{"type":"oauth_token"}}}}`, []string{"auth.profiles.p.secrets", "oauth_token"}},
		{"host login with secrets", `{"auth":{"profiles":{"p":{"type":"host_login","secrets":["A"]}}}}`, []string{"auth.profiles.p.secrets", "host_login"}},
		{"secret value instead of a name", `{"auth":{"profiles":{"p":{"type":"api_key","secrets":["sk-ant-api03-abcdefghijklmnopqrstuvwxyz"]}}}}`, []string{"auth.profiles.p.secrets", "environment variable name"}},
		{"secret-looking env value", `{"auth":{"profiles":{"p":{"type":"bedrock","env":{"AWS_SECRET_ACCESS_KEY":"aZ3kLm9QpR7sT2vXyB5nC8dE1fG4hJ6uWq0"}}}}}`, []string{"auth.profiles.p.env.AWS_SECRET_ACCESS_KEY", "secret"}},
		{"bad profile name", `{"auth":{"profiles":{"Bad Name":{"type":"host_login"}}}}`, []string{"Bad Name"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.doc, env{})
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "sk-ant-api03-abcdefghijklmnopqrstuvwxyz") || strings.Contains(err.Error(), "aZ3kLm9QpR7sT2vXyB5nC8dE1fG4hJ6uWq0") {
				t.Errorf("error echoes a secret-looking value: %v", err)
			}
		})
	}
}

func TestDerivedPathsLiveUnderRigHome(t *testing.T) {
	home := t.TempDir()
	got, err := load(t, "", env{"RIG_HOME": home})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"socket":  filepath.Join(home, "rigd.sock"),
		"pid":     filepath.Join(home, "rigd.pid"),
		"log":     filepath.Join(home, "rigd.log"),
		"store":   filepath.Join(home, "store.json"),
		"tickets": filepath.Join(home, "tickets"),
	}
	have := map[string]string{
		"socket":  got.SocketPath(),
		"pid":     got.PIDPath(),
		"log":     got.LogPath(),
		"store":   got.StorePath(),
		"tickets": got.TicketsDir(),
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("paths = %v, want %v", have, want)
	}
}
