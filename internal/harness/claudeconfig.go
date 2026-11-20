package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/fsutil"
	rigsecrets "github.com/charlesrobsampson/rig/internal/secrets"
)

// templateEntries are what a template may give an agent. Everything else in a Claude Code
// configuration directory (credentials, history, caches) belongs to its user and stays out.
var templateEntries = []struct {
	name string
	dir  bool
}{
	{"CLAUDE.md", false}, {"settings.json", false},
	{"agents", true}, {"skills", true}, {"commands", true}, {"output-styles", true}, {"hooks", true},
}

// PrepareConfig makes dir a Claude Code configuration directory an agent can start
// from without a person: first-run onboarding counts as done, and the folder-trust
// question is answered for exactly the given paths. Claude Code keys trust by the
// real path, so callers pass resolved paths. Everything else in the file, which
// Claude Code writes as it runs, is kept. The templates are linked in last.
func (c ClaudeCode) PrepareConfig(dir string, cfg Config) error {
	if err := c.CheckTemplates(cfg.Templates, cfg.Secrets); err != nil {
		return err
	}
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
	for _, p := range cfg.Trusted {
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
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return err
	}
	return linkTemplates(dir, cfg)
}

// linkTemplates rebuilds the snapshot from the templates and links each entry it holds
// into dir. What an agent changed through a link, or added, is gone after the rebuild.
func linkTemplates(dir string, cfg Config) error {
	// Last launch's links go first: the templates may have dropped an entry since.
	for _, e := range templateEntries {
		p := filepath.Join(dir, e.name)
		if info, err := os.Lstat(p); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	if cfg.Snapshot == "" {
		return nil
	}
	if len(cfg.Templates) == 0 {
		return os.RemoveAll(cfg.Snapshot)
	}
	if err := os.MkdirAll(cfg.Snapshot, 0o700); err != nil {
		return fmt.Errorf("make the agent config snapshot: %w", err)
	}
	// Emptied rather than replaced: a running agent container's mount holds on to
	// this directory, so a new one would never reach it.
	old, err := os.ReadDir(cfg.Snapshot)
	if err != nil {
		return err
	}
	for _, e := range old {
		if err := os.RemoveAll(filepath.Join(cfg.Snapshot, e.Name())); err != nil {
			return err
		}
	}
	for _, tmpl := range cfg.Templates {
		for _, e := range templateEntries {
			src := filepath.Join(tmpl, e.name)
			if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err := copyTree(src, filepath.Join(cfg.Snapshot, e.name)); err != nil {
				return fmt.Errorf("copy agent config %s: %w", src, err)
			}
		}
	}
	for _, e := range templateEntries {
		if _, err := os.Lstat(filepath.Join(cfg.Snapshot, e.name)); err != nil {
			continue
		}
		link := filepath.Join(dir, e.name)
		// A template's entry replaces what Claude Code wrote there itself.
		if err := os.RemoveAll(link); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(cfg.SnapshotSeen, e.name), link); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies src over dst, file by file, keeping whether each file is executable.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644|info.Mode().Perm()&0o111)
	})
}

// CheckTemplates refuses a template that is missing, holds links or other non-files
// (which could reach anything on the host), contains a secret rig knows, or has
// settings that would switch off or re-point the hooks rig reads the agent's state from.
func (ClaudeCode) CheckTemplates(templates []string, secrets map[string]string) error {
	for _, tmpl := range templates {
		if err := checkTemplate(tmpl, secrets); err != nil {
			return fmt.Errorf("agent config %s: %w", tmpl, err)
		}
	}
	return nil
}

func checkTemplate(tmpl string, secrets map[string]string) error {
	if !filepath.IsAbs(tmpl) {
		return errors.New("must be an absolute path")
	}
	info, err := os.Stat(tmpl)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("does not exist")
	case err != nil:
		return err
	case !info.IsDir():
		return errors.New("is not a directory")
	}
	for _, e := range templateEntries {
		root := filepath.Join(tmpl, e.name)
		info, err := os.Lstat(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil && info.Mode()&fs.ModeSymlink == 0 && info.IsDir() != e.dir {
			kind := "file"
			if e.dir {
				kind = "directory"
			}
			return fmt.Errorf("%s must be a %s", e.name, kind)
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(tmpl, path)
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				return fmt.Errorf("%s is a link; copy what it points to into the template instead", rel)
			case d.IsDir():
				return nil
			case !d.Type().IsRegular():
				return fmt.Errorf("%s is not a regular file", rel)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if name, leaked := rigsecrets.Leaked(data, secrets); leaked {
				return fmt.Errorf("%s contains the value of the secret %s", rel, name)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return checkSettings(filepath.Join(tmpl, "settings.json"))
}

func checkSettings(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var s struct {
		DisableAllHooks bool           `json:"disableAllHooks"`
		Env             map[string]any `json:"env"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("settings.json: %w", err)
	}
	if s.DisableAllHooks {
		return errors.New("settings.json sets disableAllHooks, which would also switch off the hooks rig learns the agent's state from")
	}
	for name := range s.Env {
		if strings.HasPrefix(name, "RIG_") || name == "CLAUDE_CONFIG_DIR" {
			return fmt.Errorf("settings.json sets %s in env; rig sets it, and the agent's reports depend on it", name)
		}
	}
	return nil
}
