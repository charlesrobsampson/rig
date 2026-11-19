package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/journal"
)

// The hook command runs under the container's sh (dash on Debian), whose echo would
// turn JSON escapes like \n and \\ into real characters and break a record in two.
func TestTheContainerHookWritesItsArgumentExactly(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.log")
	args := []string{
		"working",
		`{"kind":"feedback","text":"line one\nline two, a \\ backslash, a \"quote\" and \t a tab"}`,
		`{"kind":"note","text":"$HOME $(id) ` + "`id`" + ` %s %n -n"}`,
		"-n",
	}
	for _, arg := range args {
		// the harness appends the argument to the hook command, as Claude Code runs it
		cmd := exec.Command("sh", "-c", containerHookCommand(events)+` "$0"`, arg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook failed for %q: %v %s", arg, err, out)
		}
	}
	raw, _ := os.ReadFile(events)
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != len(args) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(args), raw)
	}
	for i, arg := range args {
		if lines[i] != arg {
			t.Errorf("line %d = %q, want %q", i, lines[i], arg)
		}
	}
	if e, err := journal.Parse(lines[1]); err != nil || !strings.Contains(e.Text, "line one\nline two") {
		t.Errorf("the record does not parse back: %+v, %v", e, err)
	}
}
