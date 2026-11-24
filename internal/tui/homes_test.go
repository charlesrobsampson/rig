package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

var rows = []HomeRow{
	{Name: "default", Status: "running (v1)", Tickets: "0"},
	{Name: "osmo", Status: "stopped", Tickets: "-"},
	{Name: "portal", Status: "running (v1)", Tickets: "2"},
}

func pick(t *testing.T, keys ...string) (Picker, tea.Cmd) {
	t.Helper()
	p := NewPicker(rows)
	p, _ = p.send(tea.WindowSizeMsg{Width: 80, Height: 20})
	var cmd tea.Cmd
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		default:
			msg = key(k)
		}
		p, cmd = p.send(msg)
	}
	return p, cmd
}

func TestThePickerListsEveryHomeAndOpensTheSelectedOne(t *testing.T) {
	p, _ := pick(t)
	v := p.View()
	for _, want := range []string{"default", "osmo", "stopped", "portal", "2"} {
		if !strings.Contains(v, want) {
			t.Errorf("picker lacks %q:\n%s", want, v)
		}
	}
	p, cmd := pick(t, "j", "j", "k", "enter")
	if p.Chosen() != "osmo" || !isQuit(cmd) {
		t.Errorf("chosen = %q, quit = %v", p.Chosen(), isQuit(cmd))
	}
	if p, cmd := pick(t, "G", "enter"); p.Chosen() != "portal" || !isQuit(cmd) {
		t.Errorf("G enter chose %q", p.Chosen())
	}
	if p, cmd := pick(t, "q"); p.Chosen() != "" || !isQuit(cmd) {
		t.Errorf("q chose %q", p.Chosen())
	}
}

func TestThePickerFiltersAsYouType(t *testing.T) {
	p, _ := pick(t, "/", "p", "o", "r")
	if v := p.View(); strings.Contains(v, "osmo") || strings.Contains(v, "default") || !strings.Contains(v, "portal") {
		t.Errorf("filtered on por:\n%s", v)
	}
	// while typing, letters are text, not keys: j and q go into the filter
	p, cmd := pick(t, "/", "q")
	if isQuit(cmd) || !strings.Contains(p.View(), "/q") {
		t.Errorf("q while filtering quit or was lost:\n%s", p.View())
	}
	if p, cmd := pick(t, "/", "o", "s", "enter"); p.Chosen() != "osmo" || !isQuit(cmd) {
		t.Errorf("filter then enter chose %q", p.Chosen())
	}
	p, _ = pick(t, "/", "x", "y", "esc")
	if v := p.View(); !strings.Contains(v, "osmo") || !strings.Contains(v, "portal") {
		t.Errorf("esc did not clear the filter:\n%s", v)
	}
	p, _ = pick(t, "/", "o", "x", "backspace")
	if v := p.View(); !strings.Contains(v, "osmo") {
		t.Errorf("backspace did not widen the filter:\n%s", v)
	}
	if p, cmd := pick(t, "/", "z", "z", "enter"); p.Chosen() != "" || isQuit(cmd) {
		t.Errorf("enter with nothing matching chose %q", p.Chosen())
	}
}

func deepLinked(t *testing.T, target *Target, home string, copied *[]string) Model {
	t.Helper()
	src := &fakeSource{tickets: demo, files: files, diff: "the diff"}
	m := New(Options{Source: src, Home: home, Open: target, RefreshEvery: 1, Copy: func(s string) error { *copied = append(*copied, s); return nil }})
	m, _ = send(m, tea.WindowSizeMsg{Width: 120, Height: 20})
	m, cmd := load(t, m)
	m, _ = run(t, m, cmd)
	return m
}

func TestADeepLinkOpensTheTicketOnItsTab(t *testing.T) {
	var copied []string
	m := deepLinked(t, &Target{Ticket: "demo-1", Tab: TabDiff}, "", &copied)
	if m.det == nil || m.det.ticket != "demo-1" || m.det.tab != TabDiff {
		t.Fatalf("detail = %+v", m.det)
	}
	if v := m.View(); !strings.Contains(v, "the diff") {
		t.Errorf("the diff tab is not showing:\n%s", v)
	}
	m = deepLinked(t, &Target{Ticket: "nope"}, "", &copied)
	if m.det != nil || !strings.Contains(m.View(), `no ticket "nope"`) {
		t.Errorf("an unknown ticket:\n%s", m.View())
	}
	// it opens once: a later refresh does not pull the person back
	m = deepLinked(t, &Target{Ticket: "demo-1"}, "", &copied)
	m = press(t, m, "esc")
	m, _ = send(m, loadedMsg{tickets: demo})
	if m.det != nil {
		t.Error("a refresh reopened the deep-linked ticket")
	}
}

func TestYCopiesTheCommandForWhatIsOnScreen(t *testing.T) {
	var copied []string
	m := deepLinked(t, nil, "osmo", &copied)
	m = press(t, m, "Y")
	m = press(t, m, "enter", "Y")
	m = press(t, m, "4", "Y")
	want := []string{"rig -H osmo", "rig -H osmo -t demo-1", "rig -H osmo -t demo-1 --tab diff"}
	if strings.Join(copied, "|") != strings.Join(want, "|") {
		t.Errorf("copied %q, want %q", copied, want)
	}
	if !strings.Contains(m.View(), "rig -H osmo -t demo-1 --tab diff") {
		t.Errorf("the command is not shown:\n%s", m.View())
	}
	copied = nil
	m = deepLinked(t, nil, "", &copied)
	m = press(t, m, "Y")
	if len(copied) != 1 || copied[0] != "rig" {
		t.Errorf("without a home: %q", copied)
	}
}

func TestHGoesBackToThePickerOnlyWhenThereIsOne(t *testing.T) {
	src := &fakeSource{tickets: demo}
	m := New(Options{Source: src, Home: "osmo", CanSwitch: true})
	m, _ = load(t, m)
	m, cmd := send(m, key("H"))
	if !m.SwitchHome() || !isQuit(cmd) {
		t.Errorf("H with a picker: switch %v, quit %v", m.SwitchHome(), isQuit(cmd))
	}
	m = New(Options{Source: src, Home: "osmo"})
	m, _ = load(t, m)
	m, cmd = send(m, key("H"))
	if m.SwitchHome() || isQuit(cmd) {
		t.Error("H quit although there is no other home to pick")
	}
	if v := m.View(); !strings.Contains(v, "osmo") {
		t.Errorf("the header does not name the home:\n%s", v)
	}
}
