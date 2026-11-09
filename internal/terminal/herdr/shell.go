package herdr

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellLine is what gets typed into a pane's interactive shell to start the command:
// `exec env K='v' 'cmd' 'arg'...`. Quoting every word keeps spaces, quotes and
// dollar signs exact. `exec` replaces the shell, so when the command ends the pane
// ends (Herdr then drops it) instead of dropping back to a prompt that would look
// alive. It works in POSIX shells and in fish.
func shellLine(command []string, env map[string]string) (string, error) {
	var b strings.Builder
	b.WriteString("exec")
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			if !envName.MatchString(k) {
				return "", fmt.Errorf("environment variable name %q is not valid", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" env")
		for _, k := range keys {
			b.WriteString(" " + k + "=" + quote(env[k]))
		}
	}
	for _, word := range command {
		b.WriteString(" " + quote(word))
	}
	return b.String(), nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeLaunchScript puts the launch line in a private script and returns the short
// command that runs it. The line itself cannot be typed: a shell in canonical mode
// refuses input lines over 1024 bytes (an agent's prompt is far longer), and typing it
// would also leave secrets from the environment in the pane's scrollback. The script
// deletes itself first thing, so nothing stays on disk. dir is where it was written,
// for cleanup if it never ran.
func writeLaunchScript(base, line string) (typed, dir string, err error) {
	dir, err = os.MkdirTemp(base, "rig-launch")
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, "launch.sh")
	body := "#!/bin/sh\nrm -rf " + quote(dir) + "\n" + line + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", dir, err
	}
	return "exec sh " + quote(path), dir, nil
}

// sweepLaunchScripts removes scripts older than an hour that never ran (the daemon
// that wrote them was gone before their pane was closed).
func sweepLaunchScripts(base string, now time.Time) {
	if base == "" {
		base = os.TempDir()
	}
	dirs, _ := filepath.Glob(filepath.Join(base, "rig-launch*"))
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err == nil && info.IsDir() && now.Sub(info.ModTime()) > time.Hour {
			os.RemoveAll(dir)
		}
	}
}
