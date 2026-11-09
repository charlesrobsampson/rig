package tmux

import (
	"strings"

	"github.com/charlesrobsampson/rig/internal/domain"
)

// heuristic guesses an agent's state from what Claude Code's TUI shows. It is only a
// fallback for panes without hooks (docs/spikes/03-tmux.md §2B); the strings are UI
// text and may change between versions, so anything unrecognized stays unknown.
func heuristic(screen string) domain.AgentState {
	lines := strings.Split(strings.TrimRight(screen, "\n \t"), "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:] // old output must not outvote the current screen
	}
	tail := strings.Join(lines, "\n")
	switch {
	case strings.Contains(tail, "Do you want to") && hasChoiceList(lines):
		return domain.AgentBlocked
	case strings.Contains(tail, "esc to interrupt"):
		return domain.AgentWorking
	case strings.Contains(tail, "? for shortcuts") || strings.Contains(tail, "╭─") || strings.Contains(tail, "❯"):
		return domain.AgentIdle
	}
	return domain.AgentUnknown
}

func hasChoiceList(lines []string) bool {
	for _, l := range lines {
		l = strings.TrimLeft(l, " │❯>")
		if strings.HasPrefix(l, "1.") || strings.HasPrefix(l, "1)") {
			return true
		}
	}
	return false
}

// atLeast reports whether `tmux -V` output names version major.minor or newer.
// Development builds ("next-3.4") count by their number.
func atLeast(versionOutput string, major, minor int) bool {
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(versionOutput), "tmux"))
	v = strings.TrimPrefix(strings.TrimSpace(v), "next-")
	var gotMajor, gotMinor int
	i := 0
	for ; i < len(v) && v[i] >= '0' && v[i] <= '9'; i++ {
		gotMajor = gotMajor*10 + int(v[i]-'0')
	}
	if i == 0 {
		return false
	}
	if i < len(v) && v[i] == '.' {
		for i++; i < len(v) && v[i] >= '0' && v[i] <= '9'; i++ {
			gotMinor = gotMinor*10 + int(v[i]-'0')
		}
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}
