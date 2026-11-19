package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/harness"
)

type claudeJSON struct {
	HasCompletedOnboarding bool                      `json:"hasCompletedOnboarding"`
	Theme                  string                    `json:"theme"`
	Projects               map[string]map[string]any `json:"projects"`
}

func readClaudeJSON(t *testing.T, dir string) (claudeJSON, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c claudeJSON
	var all map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &all)
	return c, all
}

func trusted(c claudeJSON) []string {
	var out []string
	for path, p := range c.Projects {
		if p["hasTrustDialogAccepted"] == true {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

func prepare(t *testing.T, dir string, paths ...string) {
	t.Helper()
	h, _ := harness.Get("claude-code")
	if err := h.PrepareConfig(dir, harness.Config{Trusted: paths}); err != nil {
		t.Fatal(err)
	}
}

func TestAConfigDirectoryIsPassedToClaudeCode(t *testing.T) {
	r := harness.Request{Ticket: ticket("api"), Service: "api", Phase: domain.PhasePlan, Dir: "/work", ConfigDir: "/rig/claude"}
	if got := launch(t, r).Env["CLAUDE_CONFIG_DIR"]; got != "/rig/claude" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want /rig/claude", got)
	}
	r.ConfigDir = ""
	if _, ok := launch(t, r).Env["CLAUDE_CONFIG_DIR"]; ok {
		t.Error("without a config directory the agent must use its user's own")
	}
}

func TestAPreparedConfigSkipsFirstRunAndTrustsExactlyTheGivenPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "claude")
	prepare(t, dir, "/work", "/rig/ticket")

	c, _ := readClaudeJSON(t, dir)
	if !c.HasCompletedOnboarding || c.Theme == "" {
		t.Errorf("config = %+v; onboarding must be done and a theme set, or the agent stops at the first-run screens", c)
	}
	if got := trusted(c); !slices.Equal(got, []string{"/rig/ticket", "/work"}) {
		t.Errorf("trusted = %v, want exactly /rig/ticket and /work", got)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode: %v, %v; want 0700", info, err)
	}
	if info, err := os.Stat(filepath.Join(dir, ".claude.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf(".claude.json mode: %v, %v; want 0600", info, err)
	}
}

func TestPreparingAgainKeepsWhatClaudeCodeWrote(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"numStartups":5,"theme":"light","userID":"u1",
		"projects":{"/work":{"lastVersionBase":"2.1.328"}}}`), 0o600)
	prepare(t, dir, "/work", "/rig/ticket")

	c, all := readClaudeJSON(t, dir)
	if all["numStartups"] != float64(5) || all["userID"] != "u1" || c.Theme != "light" {
		t.Errorf("config = %v; keys Claude Code wrote, and the user's theme, must be kept", all)
	}
	if c.Projects["/work"]["lastVersionBase"] != "2.1.328" || c.Projects["/work"]["hasTrustDialogAccepted"] != true {
		t.Errorf("/work = %v; trust is added next to what was there", c.Projects["/work"])
	}
	if got := trusted(c); !slices.Equal(got, []string{"/rig/ticket", "/work"}) {
		t.Errorf("trusted = %v", got)
	}
}

func TestADamagedConfigIsReplaced(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{half a file`), 0o600)
	prepare(t, dir, "/work")
	if c, _ := readClaudeJSON(t, dir); !c.HasCompletedOnboarding || !slices.Equal(trusted(c), []string{"/work"}) {
		t.Errorf("config = %+v", c)
	}
}

// template writes a directory laid out like ~/.claude.
func template(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func prepareWith(t *testing.T, dir string, c harness.Config) error {
	t.Helper()
	h, _ := harness.Get("claude-code")
	return h.PrepareConfig(dir, c)
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTemplatesAreCopiedToASnapshotTheConfigLinksTo(t *testing.T) {
	first := template(t, map[string]string{
		"CLAUDE.md":              "be careful",
		"settings.json":          `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"\"$CLAUDE_CONFIG_DIR/hooks/log.sh\""}]}]}}`,
		"agents/reviewer.md":     "first reviewer",
		"skills/ticket/SKILL.md": "ticket skill",
		"hooks/log.sh":           "#!/bin/sh\n",
		".credentials.json":      `{"token":"x"}`,
		"projects/p/chat.jsonl":  "history",
	})
	second := template(t, map[string]string{
		"agents/reviewer.md": "second reviewer",
		"agents/coder.md":    "coder",
	})
	dir, snapshot := filepath.Join(t.TempDir(), "claude"), filepath.Join(t.TempDir(), "snapshot")
	err := prepareWith(t, dir, harness.Config{Trusted: []string{"/work"}, Templates: []string{first, second}, Snapshot: snapshot, SnapshotSeen: "/rig/agent-config"})
	if err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(snapshot, "agents", "reviewer.md")); got != "second reviewer" {
		t.Errorf("reviewer.md = %q; a later template's file replaces an earlier one's", got)
	}
	for _, name := range []string{"CLAUDE.md", "settings.json", "agents/coder.md", "skills/ticket/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(snapshot, name)); err != nil {
			t.Errorf("%s missing from the snapshot: %v", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(snapshot, "hooks", "log.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook script: %v, %v; it must stay executable", info, err)
	}
	for _, name := range []string{".credentials.json", "projects"} {
		if _, err := os.Lstat(filepath.Join(snapshot, name)); !os.IsNotExist(err) {
			t.Errorf("%s was copied; only instructions, settings, agents, skills, commands, output styles and hooks are", name)
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s appeared in the agent's config", name)
		}
	}
	for _, name := range []string{"CLAUDE.md", "settings.json", "agents", "skills", "hooks"} {
		if target, err := os.Readlink(filepath.Join(dir, name)); err != nil || target != "/rig/agent-config/"+name {
			t.Errorf("%s -> %q, %v; want a link to the snapshot as the agent sees it", name, target, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "commands")); !os.IsNotExist(err) {
		t.Error("commands was linked though no template has it")
	}
	if got := read(t, filepath.Join(first, "agents", "reviewer.md")); got != "first reviewer" {
		t.Errorf("the template itself changed: %q", got)
	}
	if c, _ := readClaudeJSON(t, dir); !slices.Equal(trusted(c), []string{"/work"}) {
		t.Errorf("trust = %v", trusted(c))
	}
}

func TestPreparingAgainRebuildsTheSnapshotFromTheTemplates(t *testing.T) {
	tmpl := template(t, map[string]string{"agents/a.md": "a", "skills/s/SKILL.md": "s"})
	dir, snapshot := filepath.Join(t.TempDir(), "claude"), filepath.Join(t.TempDir(), "snapshot")
	cfg := harness.Config{Templates: []string{tmpl}, Snapshot: snapshot, SnapshotSeen: snapshot}
	if err := prepareWith(t, dir, cfg); err != nil {
		t.Fatal(err)
	}
	// The agent writes through its link; the template drops its skills.
	os.WriteFile(filepath.Join(dir, "agents", "a.md"), []byte("changed by the agent"), 0o644)
	os.WriteFile(filepath.Join(dir, "agents", "planted.md"), []byte("planted"), 0o644)
	os.RemoveAll(filepath.Join(tmpl, "skills"))
	// Claude Code wrote its own settings.json, which no template provides.
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"opus"}`), 0o600)

	if err := prepareWith(t, dir, cfg); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "agents", "a.md")); got != "a" {
		t.Errorf("a.md = %q; what the agent changed must be dropped at the next start", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "planted.md")); !os.IsNotExist(err) {
		t.Error("a file the agent added survived the rebuild")
	}
	if _, err := os.Lstat(filepath.Join(dir, "skills")); !os.IsNotExist(err) {
		t.Error("skills is still linked after the template dropped it")
	}
	if got := read(t, filepath.Join(dir, "settings.json")); got != `{"model":"opus"}` {
		t.Errorf("settings.json = %q; a file no template provides is Claude Code's and is kept", got)
	}

	if err := prepareWith(t, dir, harness.Config{Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "agents")); !os.IsNotExist(err) {
		t.Error("agents is still linked with no templates")
	}
	if entries, _ := os.ReadDir(snapshot); len(entries) > 0 {
		t.Errorf("snapshot still holds %v with no templates", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude.json")); err != nil {
		t.Error(".claude.json must survive")
	}
}

func TestATemplateProvidingSettingsReplacesClaudeCodesOwn(t *testing.T) {
	tmpl := template(t, map[string]string{"settings.json": `{"model":"sonnet"}`})
	dir, snapshot := t.TempDir(), filepath.Join(t.TempDir(), "snapshot")
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"opus"}`), 0o600)
	if err := prepareWith(t, dir, harness.Config{Templates: []string{tmpl}, Snapshot: snapshot, SnapshotSeen: snapshot}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "settings.json")); got != `{"model":"sonnet"}` {
		t.Errorf("settings.json = %q", got)
	}
}

func TestUnsafeTemplatesAreRefused(t *testing.T) {
	h, _ := harness.Get("claude-code")
	linked := template(t, map[string]string{"agents/a.md": "a"})
	os.Symlink("/etc/passwd", filepath.Join(linked, "agents", "passwd.md"))
	notADir := filepath.Join(template(t, map[string]string{"file": "x"}), "file")

	cases := []struct {
		name, template, want string
	}{
		{"missing", filepath.Join(t.TempDir(), "nope"), "does not exist"},
		{"relative", "configs/claude", "absolute"},
		{"a file", notADir, "not a directory"},
		{"hooks switched off", template(t, map[string]string{"settings.json": `{"disableAllHooks":true}`}), "disableAllHooks"},
		{"rig's variables", template(t, map[string]string{"settings.json": `{"env":{"RIG_TICKET":"other"}}`}), "RIG_TICKET"},
		{"the config directory", template(t, map[string]string{"settings.json": `{"env":{"CLAUDE_CONFIG_DIR":"/x"}}`}), "CLAUDE_CONFIG_DIR"},
		{"damaged settings", template(t, map[string]string{"settings.json": `{half`}), "settings.json"},
		{"a link", linked, "link"},
		{"a known secret", template(t, map[string]string{"skills/s/SKILL.md": "token: sk-live-1234567890"}), "WORK_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := h.CheckTemplates([]string{c.template}, map[string]string{"WORK_TOKEN": "sk-live-1234567890"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
			if c.name == "a known secret" && strings.Contains(err.Error(), "sk-live") {
				t.Errorf("the error shows the secret: %v", err)
			}
		})
	}

	t.Run("prepare refuses too and installs nothing", func(t *testing.T) {
		bad := template(t, map[string]string{"agents/a.md": "a", "settings.json": `{"disableAllHooks":true}`})
		dir, snapshot := t.TempDir(), filepath.Join(t.TempDir(), "snapshot")
		err := prepareWith(t, dir, harness.Config{Templates: []string{bad}, Snapshot: snapshot, SnapshotSeen: snapshot})
		if err == nil {
			t.Fatal("an unsafe template was installed")
		}
		if _, err := os.Lstat(filepath.Join(dir, "agents")); !os.IsNotExist(err) {
			t.Error("agents was linked from a refused template")
		}
	})
}
