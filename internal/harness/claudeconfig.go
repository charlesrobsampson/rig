package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/charlesrobsampson/rig/internal/fsutil"
)

// PrepareConfig makes dir a Claude Code configuration directory an agent can start
// from without a person: first-run onboarding counts as done, and the folder-trust
// question is answered for exactly the given paths. Claude Code keys trust by the
// real path, so callers pass resolved paths. Everything else in the file, which
// Claude Code writes as it runs, is kept.
func (ClaudeCode) PrepareConfig(dir string, trusted []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make the agent's Claude config directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, ".claude.json")
	config := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &config) != nil || config == nil {
			config = map[string]any{} // damaged: rig owns this directory, so start over
		}
	}
	config["hasCompletedOnboarding"] = true
	if theme, _ := config["theme"].(string); theme == "" {
		config["theme"] = "dark"
	}
	projects, _ := config["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	for _, p := range trusted {
		project, _ := projects[p].(map[string]any)
		if project == nil {
			project = map[string]any{}
		}
		project["hasTrustDialogAccepted"] = true
		projects[p] = project
	}
	config["projects"] = projects
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path, data)
}
