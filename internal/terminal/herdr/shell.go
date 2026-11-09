package herdr

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
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
