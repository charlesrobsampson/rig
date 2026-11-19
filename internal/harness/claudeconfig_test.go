package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	if err := h.PrepareConfig(dir, paths); err != nil {
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
