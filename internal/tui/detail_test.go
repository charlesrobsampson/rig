package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
)

// run executes a command and feeds its messages (and those of any batch) back into the
// model. refreshMsg timers are returned instead of followed, so a test steps them by
// hand and nothing loops.
func run(t *testing.T, m Model, cmd tea.Cmd) (Model, []refreshMsg) {
	t.Helper()
	var timers []refreshMsg
	var step func(c tea.Cmd)
	step = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch msg := c().(type) {
		case tea.BatchMsg:
			for _, sub := range msg {
				step(sub)
			}
		case refreshMsg:
			timers = append(timers, msg)
		case nil:
		default:
			var next tea.Cmd
			m, next = send(m, msg)
			step(next)
		}
	}
	step(cmd)
	return m, timers
}

var files = map[string]string{
	"plan.md":     "# Plan\n\nAdd a locale header.\n",
	"tasks.md":    "# Tasks\n- [x] add header\n- [ ] add test\n- [X] docs\n  - [ ] nested\n",
	"contract.md": "# Contract\n## Agreed\nGET /x\n## Proposed changes\n### P-api-1 · from api · open\n**What:** w\n**Why:** y\n### P-web-1 · from web · accepted\n",
}

func opened(t *testing.T, src *fakeSource) Model {
	t.Helper()
	m := New(Options{Source: src, RefreshEvery: time.Millisecond})
	m, _ = send(m, tea.WindowSizeMsg{Width: 80, Height: 14})
	m, _ = load(t, m)
	m, cmd := send(m, key("enter"))
	m, _ = run(t, m, cmd)
	return m
}

func TestEnterOpensTheSelectedTicketOnThePlanAndEscComesBack(t *testing.T) {
	src := &fakeSource{tickets: demo, files: files}
	m := opened(t, src)
	v := m.View()
	for _, want := range []string{"demo-1", "[plan]", "Add a locale header."} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "TICKET") {
		t.Error("the dashboard is still showing")
	}
	m, _ = send(m, key("esc"))
	if m.det != nil || !strings.Contains(m.View(), "TICKET") {
		t.Errorf("esc did not return to the dashboard:\n%s", m.View())
	}
}

func TestEnterOnAnEmptyFleetDoesNothing(t *testing.T) {
	m, _ := load(t, New(Options{Source: &fakeSource{}}))
	if m2, cmd := send(m, key("enter")); m2.det != nil || cmd != nil {
		t.Error("opened a ticket that is not there")
	}
}

func TestTabsSwitchLoadOnceAndWrap(t *testing.T) {
	src := &fakeSource{tickets: demo, files: files, diff: "diff --git a/x b/x\n+new\n", logs: "api │ started\n"}
	m := opened(t, src)
	step := func(k string) {
		var cmd tea.Cmd
		m, cmd = send(m, key(k))
		m, _ = run(t, m, cmd)
	}
	step("tab")
	if v := m.View(); !strings.Contains(v, "[tasks 2/4]") || !strings.Contains(v, "add header") {
		t.Errorf("tasks tab:\n%s", v)
	}
	step("3")
	if v := m.View(); !strings.Contains(v, "[contract (1 open)]") || !strings.Contains(v, "GET /x") {
		t.Errorf("contract tab:\n%s", v)
	}
	step("4")
	if v := m.View(); !strings.Contains(v, "[diff]") || !strings.Contains(v, "+new") {
		t.Errorf("diff tab:\n%s", v)
	}
	step("5")
	if v := m.View(); !strings.Contains(v, "[logs]") || !strings.Contains(v, "started") {
		t.Errorf("logs tab:\n%s", v)
	}
	step("tab") // manual
	step("tab") // wraps to plan
	if m.det.tab != TabPlan {
		t.Errorf("tab = %v after wrapping", m.det.tab)
	}
	m, cmd := send(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	m, _ = run(t, m, cmd)
	if m.det.tab != TabManual {
		t.Errorf("shift+tab from plan = %v, want manual", m.det.tab)
	}
	before := len(src.reads)
	step("1")
	step("5")
	step("6")
	if len(src.reads) != before {
		t.Errorf("tabs already loaded were read again: %v", src.reads[before:])
	}
}

func TestTaskProgressCountsCheckboxes(t *testing.T) {
	tests := []struct {
		md          string
		done, total int
	}{
		{"", 0, 0},
		{"no boxes here\n- plain item\n", 0, 0},
		{"- [ ] a\n- [x] b\n- [X] c\n", 2, 3},
		{"* [x] star\n  - [ ] nested\n\t- [x] tabbed\n", 2, 3},
		{"- [] not a box\n- [y] not a box\n-[x] no space\n", 0, 0},
		{"text - [x] mid-line is not a task\n", 0, 0},
	}
	for _, tt := range tests {
		if d, n := TaskProgress(tt.md); d != tt.done || n != tt.total {
			t.Errorf("TaskProgress(%q) = %d/%d, want %d/%d", tt.md, d, n, tt.done, tt.total)
		}
	}
	m := opened(t, &fakeSource{tickets: demo, files: files})
	m, cmd := send(m, key("2"))
	m, _ = run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "██████████░░░░░░░░░░  2/4 done") {
		t.Errorf("progress bar:\n%s", v)
	}
}

func TestMissingFilesSayWhyAndFailuresAreShown(t *testing.T) {
	src := &fakeSource{tickets: demo, files: map[string]string{}}
	m := opened(t, src)
	if v := m.View(); !strings.Contains(v, "plan.md does not exist yet") {
		t.Errorf("plan:\n%s", v)
	}
	for k, want := range map[string]string{
		"3": "no contract.md; only tickets with several services",
		"4": "no checkouts",
	} {
		var cmd tea.Cmd
		m, cmd = send(m, key(k))
		m, _ = run(t, m, cmd)
		if v := m.View(); !strings.Contains(v, want) {
			t.Errorf("tab %s lacks %q:\n%s", k, want, v)
		}
	}
	src.fileErr = errors.New("daemon said no")
	m, cmd := send(m, key("5"))
	m, _ = run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "Could not load logs") || !strings.Contains(v, "daemon said no") {
		t.Errorf("logs:\n%s", v)
	}
	// r retries a failed tab
	src.fileErr, src.logs = nil, "now fine"
	m, cmd = send(m, key("r"))
	m, _ = run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "now fine") {
		t.Errorf("after r:\n%s", v)
	}
}

func TestScrollingIsBoundedAndKeptPerTab(t *testing.T) {
	var long strings.Builder
	for i := 0; i < 50; i++ {
		long.WriteString("line " + string(rune('A'+i/26)) + string(rune('a'+i%26)) + "\n")
	}
	src := &fakeSource{tickets: demo, files: map[string]string{"plan.md": long.String(), "tasks.md": "- [ ] x\n"}}
	m := opened(t, src) // height 14: 14 - header - status - tab bar = 11 body lines
	press := func(k tea.KeyMsg) { m, _ = send(m, k) }

	if v := m.View(); !strings.Contains(v, "line Aa") || strings.Contains(v, "line Al") {
		t.Fatalf("top of the plan:\n%s", v)
	}
	press(key("j"))
	press(key("j"))
	if v := m.View(); strings.Contains(v, "line Aa") || !strings.Contains(v, "line Ac") {
		t.Errorf("after two j:\n%s", v)
	}
	press(key("k"))
	press(key("k"))
	press(key("k"))
	press(key("k")) // past the top
	if v := m.View(); !strings.Contains(v, "line Aa") {
		t.Errorf("scrolled above the top:\n%s", v)
	}
	press(tea.KeyMsg{Type: tea.KeyPgDown})
	if v := m.View(); !strings.Contains(v, "line Al") {
		t.Errorf("page down:\n%s", v)
	}
	press(key("G"))
	for range 5 {
		press(key("j")) // past the bottom
	}
	v := m.View()
	if !strings.Contains(v, "line Bw") || strings.Contains(v, "line Aa") {
		t.Errorf("bottom:\n%s", v)
	}
	if n := len(strings.Split(v, "\n")); n != 14 {
		t.Errorf("%d lines at the bottom, want a full screen of 14", n)
	}
	// the position belongs to the tab: tasks starts at the top, plan keeps its place
	m, cmd := send(m, key("2"))
	m, _ = run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "[tasks 0/1]") {
		t.Errorf("tasks:\n%s", v)
	}
	m, _ = send(m, key("1"))
	if v := m.View(); !strings.Contains(v, "line Bw") {
		t.Errorf("plan lost its scroll position:\n%s", v)
	}
	press(key("g"))
	if v := m.View(); !strings.Contains(v, "line Aa") {
		t.Errorf("g:\n%s", v)
	}
}

func TestShortContentDoesNotScrollAndLongLinesWrap(t *testing.T) {
	src := &fakeSource{tickets: demo, files: map[string]string{"plan.md": "short\n" + strings.Repeat("w", 200) + "\n"}}
	m := opened(t, src)
	for range 10 {
		m, _ = send(m, key("j"))
	}
	m, _ = send(m, tea.KeyMsg{Type: tea.KeyPgDown})
	v := m.View()
	if !strings.Contains(v, "short") {
		t.Errorf("scrolled short content away:\n%s", v)
	}
	for _, line := range strings.Split(v, "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("line of %d columns: %q", n, line)
		}
	}
	if strings.Count(v, "w") < 200 {
		t.Error("wrapping lost characters")
	}
}

func TestTheOpenTabFollowsChangesOnTheRefreshTimer(t *testing.T) {
	src := &fakeSource{tickets: demo, files: map[string]string{"plan.md": "version one\n"}}
	m := opened(t, src)
	src.files["plan.md"] = "version two\n"
	m, cmd := send(m, refreshMsg{ticket: "demo-1", gen: m.det.gen})
	m, timers := run(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "version two") || strings.Contains(v, "version one") {
		t.Errorf("the tab did not follow the file:\n%s", v)
	}
	if len(timers) != 1 {
		t.Errorf("the next refresh was not scheduled: %v", timers)
	}
}

func TestStaleTimersAndLoadsAreIgnored(t *testing.T) {
	src := &fakeSource{tickets: []api.TicketSummary{{ID: "a"}, {ID: "b"}}, files: map[string]string{"plan.md": "plan\n"}}
	m := opened(t, src) // opens "a"
	firstGen := m.det.gen
	m, _ = send(m, key("esc"))
	// a timer from the closed view does nothing and does not reschedule
	if _, cmd := send(m, refreshMsg{ticket: "a", gen: firstGen}); cmd != nil {
		t.Error("a timer for a closed ticket kept running")
	}
	// reopening makes a new generation; the old timer is still dead
	m, cmd := send(m, key("enter"))
	m, _ = run(t, m, cmd)
	if m.det.gen == firstGen {
		t.Fatal("a reopened ticket kept the old generation")
	}
	if _, cmd := send(m, refreshMsg{ticket: "a", gen: firstGen}); cmd != nil {
		t.Error("an old timer revived on a reopened view")
	}
	// a slow load for another ticket must not land in this one
	m, _ = send(m, tabLoadedMsg{ticket: "b", gen: m.det.gen, tab: TabPlan, text: "WRONG"})
	m, _ = send(m, tabLoadedMsg{ticket: "a", gen: firstGen, tab: TabPlan, text: "WRONG"})
	if strings.Contains(m.View(), "WRONG") {
		t.Errorf("a stale load was shown:\n%s", m.View())
	}
}

func TestQuitAndHelpWorkInsideADetailView(t *testing.T) {
	m := opened(t, &fakeSource{tickets: demo, files: files})
	h, _ := send(m, key("?"))
	if v := h.View(); !strings.Contains(v, "Keys") {
		t.Errorf("help:\n%s", v)
	}
	h, _ = send(h, key("esc"))
	if h.det == nil {
		t.Error("esc closed the ticket instead of only the help")
	}
	if _, cmd := send(m, key("q")); !isQuit(cmd) {
		t.Error("q does not quit from a ticket")
	}
}

func TestADetailViewOnATinyScreenDoesNotPanic(t *testing.T) {
	for _, size := range [][2]int{{0, 0}, {1, 1}, {5, 3}, {10, 4}, {200, 2}} {
		m := opened(t, &fakeSource{tickets: demo, files: files})
		m, _ = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, k := range []string{"j", "G", "tab", "pgdown", "g", "esc"} {
			m, _ = send(m, key(k))
			_ = m.View()
		}
	}
}
