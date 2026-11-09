package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecretsConfig(t *testing.T, home, envFileContent string) (envFile string) {
	t.Helper()
	envFile = filepath.Join(home, "secrets.env")
	if envFileContent != "" {
		os.WriteFile(envFile, []byte(envFileContent), 0o600)
	}
	cfg := filepath.Join(home, "config.json")
	os.WriteFile(cfg, []byte(`{"secrets":{"keychain_service":"rig-test","env_file":"`+envFile+`"}}`), 0o600)
	t.Setenv("RIG_CONFIG", cfg)
	return envFile
}

func TestStartGivesTheAgentASecretFromTheEnvFileAndWritesItNowhere(t *testing.T) {
	home, d, repo, term := newEnvWithTerminal(t)
	const sentinel = "token-sentinel-77"
	writeSecretsConfig(t, home, "export APP_TOKEN="+sentinel+"\n")
	newTicket(t, d, "demo", repo)
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	os.WriteFile(yamlPath, append(raw, []byte("secrets: [APP_TOKEN]\n")...), 0o600)

	stdout, stderr, code := rig(t, d, "start", "demo", "--json")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if got := term.Spawned()[0].Env["APP_TOKEN"]; got != sentinel {
		t.Errorf("the agent's APP_TOKEN = %q", got)
	}
	if strings.Contains(stdout, sentinel) || strings.Contains(stderr, sentinel) {
		t.Errorf("the value is in the command output:\n%s\n%s", stdout, stderr)
	}
	shown, _, _ := rig(t, d, "config", "show")
	if strings.Contains(shown, sentinel) {
		t.Error("`rig config show` printed a value")
	}
	// nothing under the rig home, apart from the .env file itself, holds the value
	filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || p == filepath.Join(home, "secrets.env") || strings.Contains(p, "/api/") || strings.Contains(p, "/svc1/") {
			return nil
		}
		if data, _ := os.ReadFile(p); strings.Contains(string(data), sentinel) {
			t.Errorf("the secret value was written to %s", p)
		}
		return nil
	})
}

func TestStartWithAMissingSecretFailsNamingIt(t *testing.T) {
	home, d, repo, term := newEnvWithTerminal(t)
	writeSecretsConfig(t, home, "OTHER=1\n")
	newTicket(t, d, "demo", repo)
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	os.WriteFile(yamlPath, append(raw, []byte("secrets: [APP_TOKEN, DB_PASSWORD]\n")...), 0o600)

	_, stderr, code := rig(t, d, "start", "demo")
	if code != 2 || !strings.Contains(stderr, "APP_TOKEN, DB_PASSWORD") || !strings.Contains(stderr, `Keychain service "rig-test"`) || !strings.Contains(stderr, "secrets.env") {
		t.Errorf("code %d (want 2), stderr %q", code, stderr)
	}
	if len(term.Spawned()) != 0 {
		t.Error("an agent was started")
	}
	if _, err := os.Stat(filepath.Join(home, "tickets", "demo", "svc1")); !os.IsNotExist(err) {
		t.Error("a checkout was created before the secrets were checked")
	}
}

func TestAnEnvFileThatOthersCanReadIsRefused(t *testing.T) {
	home, d, repo, _ := newEnvWithTerminal(t)
	envFile := writeSecretsConfig(t, home, "APP_TOKEN=x\n")
	os.Chmod(envFile, 0o644)
	newTicket(t, d, "demo", repo)
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	os.WriteFile(yamlPath, append(raw, []byte("secrets: [APP_TOKEN]\n")...), 0o600)
	_, stderr, code := rig(t, d, "start", "demo")
	if code == 0 || !strings.Contains(stderr, "chmod 600") {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
}
