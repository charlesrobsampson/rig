package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run executes the real container status-line command with the given status JSON on its
// standard input, writing to a temporary events file, and returns what it appended.
func runCostCommand(t *testing.T, status string) (appended string, stdout string) {
	t.Helper()
	events := filepath.Join(t.TempDir(), "events.log")
	cmd := exec.Command("sh", "-c", containerCostCommand(events))
	cmd.Stdin = strings.NewReader(status)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the command failed: %v", err)
	}
	raw, _ := os.ReadFile(events)
	return string(raw), string(out)
}

func TestTheContainerStatusLineAppendsTheCostFromClaudeCodesStatusJSON(t *testing.T) {
	tests := []struct {
		name, status, want string
	}{
		{"compact JSON", `{"session_id":"s","cost":{"total_cost_usd":0.4321,"total_duration_ms":1200}}`, "cost 0.4321\n"},
		{"a space after the colon", `{"cost": {"total_cost_usd": 12.5, "total_lines_added": 3}}`, "cost 12.5\n"},
		{"pretty-printed over several lines", "{\n  \"model\": {\"id\": \"x\"},\n  \"cost\": {\n    \"total_cost_usd\": 3.07,\n    \"total_duration_ms\": 5\n  }\n}\n", "cost 3.07\n"},
		{"zero", `{"cost":{"total_cost_usd":0}}`, "cost 0\n"},
		{"scientific notation", `{"cost":{"total_cost_usd":1.5e-05}}`, "cost 1.5e-05\n"},
		{"no cost in the status", `{"session_id":"s","model":{"id":"m"}}`, ""},
		{"not JSON at all", `hello world`, ""},
		{"empty input", ``, ""},
	}
	for _, tt := range tests {
		got, stdout := runCostCommand(t, tt.status)
		if got != tt.want {
			t.Errorf("%s: appended %q, want %q", tt.name, got, tt.want)
		}
		if stdout != "" {
			t.Errorf("%s: printed %q; the status line must stay quiet", tt.name, stdout)
		}
	}
}

func TestTheContainerStatusLineCannotBeTrickedIntoAnythingButACostLine(t *testing.T) {
	// whatever the status JSON says, only "cost <number>" lines can reach the events file
	for _, status := range []string{
		`{"cost":{"total_cost_usd":1.0;rm -rf /}}`,
		"{\"cost\":{\"total_cost_usd\":\"$(id)\"}}",
		`{"cost":{"total_cost_usd":` + "`id`" + `}}`,
		`{"cost":{"total_cost_usd":working}}`,
	} {
		got, _ := runCostCommand(t, status)
		for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
			if line != "" && !strings.HasPrefix(line, "cost ") {
				t.Errorf("status %q put %q into the events file", status, line)
			}
			if strings.ContainsAny(line, ";$`") {
				t.Errorf("status %q put shell syntax into the events file: %q", status, line)
			}
		}
	}
}
