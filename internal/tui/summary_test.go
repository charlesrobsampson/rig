package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func withSummary(id string, s *api.SummaryInfo) api.TicketSummary {
	return api.TicketSummary{ID: id, Phase: domain.PhaseImplement, AISummary: s}
}

var local = &api.SummaryInfo{Text: "- adds a locale header\n- tests are missing", Backend: "ollama", Model: "llama3:8b", Files: 3, Added: 45, Deleted: 12}
var remote = &api.SummaryInfo{Text: "- refactors the router", Backend: "claude", Model: "claude-haiku-5-5", Remote: true, Files: 7, Added: 100, Deleted: 80}

func tall(m Model) Model {
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func TestTheSelectedTicketsSummaryShowsAdvisoryWithItsDiffStats(t *testing.T) {
	m := tall(ready(t, &fakeSource{}, withSummary("alpha", local), api.TicketSummary{ID: "beta"}))
	v := m.View()
	for _, want := range []string{"summary (advisory)", "ollama llama3:8b", "3 files +45 -12", "- adds a locale header", "- tests are missing"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "REMOTE") || strings.Contains(v, "remote model") {
		t.Errorf("a local backend is marked remote:\n%s", v)
	}
	// the raw table is still there beside it
	if !strings.Contains(v, "TICKET") || !strings.Contains(v, "alpha") || !strings.Contains(v, "beta") {
		t.Errorf("the table is gone:\n%s", v)
	}
	// moving to a ticket without a summary removes the pane
	m = press(t, m, "j")
	if v := m.View(); strings.Contains(v, "summary (advisory)") {
		t.Errorf("the pane stayed on a ticket with no summary:\n%s", v)
	}
}

func TestARemoteBackendIsMarkedInThePaneAndTheHeader(t *testing.T) {
	m := tall(ready(t, &fakeSource{}, withSummary("alpha", remote), withSummary("beta", local)))
	v := m.View()
	if !strings.Contains(v, "claude claude-haiku-5-5 · REMOTE") {
		t.Errorf("the pane does not say remote:\n%s", v)
	}
	if !strings.Contains(strings.Split(v, "\n")[0], "summaries from a remote model (claude): ticket content leaves this machine") {
		t.Errorf("the header does not say remote:\n%s", strings.Split(v, "\n")[0])
	}
	// only local summaries: no indicator anywhere
	m = tall(ready(t, &fakeSource{}, withSummary("alpha", local)))
	if v := m.View(); strings.Contains(v, "remote") || strings.Contains(v, "REMOTE") {
		t.Errorf("a local-only fleet shows a remote indicator:\n%s", v)
	}
	// no summaries at all: nothing extra
	m = tall(ready(t, &fakeSource{}, api.TicketSummary{ID: "alpha"}))
	if v := m.View(); strings.Contains(v, "summary") || strings.Contains(v, "remote") {
		t.Errorf("view:\n%s", v)
	}
}

func TestAFailedRefreshIsShownBesideTheOldSummary(t *testing.T) {
	stale := *local
	stale.Error = "ollama at localhost:11434: connection refused"
	m := tall(ready(t, &fakeSource{}, withSummary("alpha", &stale)))
	if v := m.View(); !strings.Contains(v, "- adds a locale header") || !strings.Contains(v, "out of date, refresh failed: ollama at localhost:11434: connection refused") {
		t.Errorf("view:\n%s", v)
	}
	onlyErr := &api.SummaryInfo{Backend: "ollama", Error: "no model"}
	m = tall(ready(t, &fakeSource{}, withSummary("alpha", onlyErr)))
	if v := m.View(); !strings.Contains(v, "summary refresh failed: no model") || strings.Contains(v, "out of date") {
		t.Errorf("view:\n%s", v)
	}
}

func TestALongSummaryIsCutAndNeverCrowdsOutTheTable(t *testing.T) {
	var text strings.Builder
	for i := 0; i < 40; i++ {
		text.WriteString("- bullet number " + string(rune('A'+i%26)) + "\n")
	}
	long := &api.SummaryInfo{Text: text.String(), Backend: "ollama", Files: 1}
	var fleet []api.TicketSummary
	fleet = append(fleet, withSummary("alpha", long))
	for _, id := range []string{"b", "c", "d", "e"} {
		fleet = append(fleet, api.TicketSummary{ID: id})
	}
	m := ready(t, &fakeSource{}, fleet...)
	m, _ = send(m, tea.WindowSizeMsg{Width: 100, Height: 16})
	v := m.View()
	if n := len(strings.Split(v, "\n")); n > 16 {
		t.Errorf("%d lines on a 16-line screen", n)
	}
	if !strings.Contains(v, "…") || strings.Count(v, "bullet number") > maxSummaryLines {
		t.Errorf("the summary was not cut:\n%s", v)
	}
	for _, id := range []string{"alpha", "b", "c", "d", "e"} {
		if !strings.Contains(v, id) {
			t.Errorf("ticket %s was pushed off the screen:\n%s", id, v)
		}
	}
}

func TestOnASmallScreenTheSummaryStepsAside(t *testing.T) {
	m := ready(t, &fakeSource{}, withSummary("alpha", local))
	m, _ = send(m, tea.WindowSizeMsg{Width: 80, Height: 10})
	v := m.View()
	if strings.Contains(v, "summary (advisory)") || !strings.Contains(v, "alpha") {
		t.Errorf("view:\n%s", v)
	}
	if n := len(strings.Split(v, "\n")); n > 10 {
		t.Errorf("%d lines", n)
	}
}

func TestAWideSummaryWrapsAndNarrowScreensDoNotPanic(t *testing.T) {
	wide := &api.SummaryInfo{Text: strings.Repeat("word ", 80), Backend: "claude", Remote: true, Error: strings.Repeat("e", 300)}
	for _, size := range [][2]int{{1, 1}, {10, 14}, {40, 20}, {200, 60}} {
		m := ready(t, &fakeSource{}, withSummary("alpha", wide))
		m, _ = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		v := m.View()
		for _, line := range strings.Split(v, "\n") {
			if n := len([]rune(line)); n > max(size[0], 1) {
				t.Errorf("%dx%d: a line of %d columns: %q", size[0], size[1], n, line)
			}
		}
	}
}
