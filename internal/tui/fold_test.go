package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func cost(v float64) *float64 { return &v }

var foldFleet = func() []api.TicketSummary {
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	d := t0.Add(time.Hour)
	return []api.TicketSummary{
		{ID: "d1", Phase: domain.PhaseDone, CostUSD: cost(1.5), AgentSeconds: 600, CreatedAt: &t0, DoneAt: &d},
		{ID: "d2", Phase: domain.PhaseDone, CostUSD: cost(2.5), AgentSeconds: 600, CreatedAt: &t0, DoneAt: &d},
		{ID: "i1", Phase: domain.PhaseImplement, CostUSD: cost(2), AgentSeconds: 3000, CreatedAt: &t0},
		{ID: "i2", Phase: domain.PhaseImplement, CostUSD: cost(2.12), AgentSeconds: 1800, CreatedAt: &t0},
		{ID: "p1", Phase: domain.PhasePlan, CreatedAt: &t0},
	}
}()

func foldModel(t *testing.T, opts ...func(*Options)) Model {
	t.Helper()
	o := Options{Source: &fakeSource{tickets: foldFleet}, Now: func() time.Time { return time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC) }}
	for _, f := range opts {
		f(&o)
	}
	m, _ := load(t, New(o))
	return m
}

// where says what the cursor is on: a ticket id, or "header:<phase>".
func where(m Model) string {
	if m.dash.selected != "" {
		return m.dash.selected
	}
	return "header:" + string(m.dash.header)
}

func TestTicketsAreGroupedByPhaseWithDoneFolded(t *testing.T) {
	m, _ := send(foldModel(t), tea.WindowSizeMsg{Width: 120, Height: 20})
	v := m.View()
	want := []string{"▾ plan (1)", "p1", "▾ implement (2)", "$4.12", "1h20m", "▸ done (2)", "$4.00", "all (5)", "$8.12", "1h40m"}
	last := -1
	for _, w := range want {
		i := strings.Index(v, w)
		if i < last || i < 0 {
			t.Fatalf("lacks %q, in order:\n%s", w, v)
		}
		last = i
	}
	if strings.Contains(v, "d1") {
		t.Errorf("done tickets show though it is folded:\n%s", v)
	}
}

func TestTheCursorSkipsFoldedRowsAndFoldKeysWork(t *testing.T) {
	m := foldModel(t)
	if where(m) != "p1" {
		t.Fatalf("starts on %s", where(m))
	}
	for _, step := range []struct{ key, want string }{
		{"j", "header:implement"}, {"j", "i1"}, {"j", "i2"}, {"j", "header:done"}, {"j", "header:done"}, // d1, d2 are folded away
		{"enter", "header:done"}, // unfolds
		{"j", "d1"}, {"j", "d2"}, {"j", "d2"},
		{"k", "d1"}, {"k", "header:done"},
		{"z", "header:done"}, // folds again
		{"j", "header:done"},
		{"g", "p1"}, {"G", "i2"}, // the last visible ticket
	} {
		m = press(t, m, step.key)
		if where(m) != step.want {
			t.Fatalf("after %s: on %s, want %s", step.key, where(m), step.want)
		}
		if step.key == "enter" && !strings.Contains(m.View(), "d1") {
			t.Fatalf("enter on the done header did not show its rows:\n%s", m.View())
		}
	}
}

func TestZFoldsTheGroupOfTheTicketUnderTheCursorAndMovesToItsHeader(t *testing.T) {
	m := press(t, foldModel(t), "j", "j", "z") // on i1
	if where(m) != "header:implement" || strings.Contains(m.View(), "i1") || !strings.Contains(m.View(), "▸ implement (2)") {
		t.Errorf("on %s:\n%s", where(m), m.View())
	}
	m = press(t, m, "space") // space on a header unfolds
	if !strings.Contains(m.View(), "i1") || where(m) != "header:implement" {
		t.Errorf("space on a header:\n%s", m.View())
	}
}

func TestShiftZFoldsAllButTheCursorsAndOOpensAll(t *testing.T) {
	m := press(t, foldModel(t), "j", "j", "Z") // on i1
	v := m.View()
	if !strings.Contains(v, "▸ plan") || !strings.Contains(v, "▾ implement") || !strings.Contains(v, "▸ done") || strings.Contains(v, "p1") || where(m) != "i1" {
		t.Errorf("after Z, on %s:\n%s", where(m), v)
	}
	m = press(t, m, "o")
	v = m.View()
	if strings.Contains(v, "▸") || !strings.Contains(v, "d1") || !strings.Contains(v, "p1") {
		t.Errorf("after o:\n%s", v)
	}
}

func TestActionsDoNothingOnAHeader(t *testing.T) {
	src := &fakeSource{}
	m := ready(t, src, api.TicketSummary{ID: "a", Phase: domain.PhasePlan, PendingGate: domain.GatePlanApproval, Services: []api.ServiceSummary{{Name: "api"}}})
	m = press(t, m, "k", "a", "y") // the header
	if len(src.acts) != 0 || where(m) != "header:plan" {
		t.Errorf("acts %v on %s", src.acts, where(m))
	}
}

func TestFoldsSurviveARefreshAndAVesselSwitch(t *testing.T) {
	folds := map[string]Folds{}
	m := foldModel(t, func(o *Options) { o.Folds, o.Vessel = folds, "work" })
	m = press(t, m, "j", "j", "z", "r") // fold implement, refresh
	m, _ = load(t, m)
	if strings.Contains(m.View(), "i1") || !strings.Contains(m.View(), "▸ implement") {
		t.Fatalf("a refresh unfolded the group:\n%s", m.View())
	}
	again := foldModel(t, func(o *Options) { o.Folds, o.Vessel = folds, "work" })
	if strings.Contains(again.View(), "i1") {
		t.Errorf("the same vessel did not keep its folds:\n%s", again.View())
	}
	other := foldModel(t, func(o *Options) { o.Folds, o.Vessel = folds, "home" })
	if !strings.Contains(other.View(), "i1") {
		t.Errorf("another vessel took work's folds:\n%s", other.View())
	}
}

func TestACursorOnATicketWhoseGroupIsFoldedByARefreshFallsToTheHeader(t *testing.T) {
	m := foldModel(t)
	m = press(t, m, "j", "j") // i1
	m.dash.folds[domain.PhaseImplement] = true
	m.dash.set(m.tickets)
	if where(m) != "header:implement" {
		t.Errorf("on %s", where(m))
	}
}

func TestHelpNamesTheFoldKeys(t *testing.T) {
	m := press(t, foldModel(t), "?")
	if v := m.View(); !strings.Contains(v, "z Z o") || !strings.Contains(v, "fold") {
		t.Errorf("help:\n%s", v)
	}
}

// colStarts are the columns of the table's header line.
func colStarts(t *testing.T, v string, names ...string) []int {
	t.Helper()
	var out []int
	for _, n := range names {
		i := strings.Index(strings.Split(v, "\n")[1], n+" ")
		if i < 0 {
			t.Fatalf("no column %s in:\n%s", n, v)
		}
		out = append(out, i)
	}
	return out
}

func TestGroupHeaderAndFooterTotalsSitUnderCostTimeAndAgent(t *testing.T) {
	var small []api.TicketSummary // short labels, so all the columns fit in 80
	for _, k := range foldFleet {
		if k.Phase != domain.PhaseImplement {
			small = append(small, k)
		}
	}
	cases := []struct {
		width int
		fleet []api.TicketSummary
		rows  map[string][3]string
	}{
		{120, foldFleet, map[string][3]string{
			"▾ implement (2)": {"$4.12", "2h00m", "1h20m"},
			"▸ done (2)":      {"$4.00", "1h00m", "20m"},
			"all (5)":         {"$8.12", "2h00m", "1h40m"},
		}},
		{80, small, map[string][3]string{
			"▸ done (2)": {"$4.00", "1h00m", "20m"},
			"all (3)":    {"$4.00", "2h00m", "20m"},
		}},
	}
	for _, c := range cases {
		m := foldModel(t, func(o *Options) { o.Source = &fakeSource{tickets: c.fleet} })
		m, _ = send(m, tea.WindowSizeMsg{Width: c.width, Height: 20})
		v := m.View()
		cols := colStarts(t, v, "COST", "TIME", "AGENT")
		for prefix, want := range c.rows {
			var l string
			for _, x := range strings.Split(v, "\n") {
				if strings.HasPrefix(strings.TrimLeft(x, "> "), prefix) {
					l = x
				}
			}
			if l == "" {
				t.Fatalf("width %d: no %q row in:\n%s", c.width, prefix, v)
			}
			r := []rune(l)
			for i, col := range cols {
				if col >= len(r) || !strings.HasPrefix(string(r[col:]), want[i]) {
					t.Errorf("width %d: %q lacks %q at column %d:\n%s", c.width, prefix, want[i], col, v)
				}
			}
		}
	}
}

func TestAtANarrowWidthTheTimeColumnGoesAndTheTotalsStayAligned(t *testing.T) {
	m, _ := send(foldModel(t), tea.WindowSizeMsg{Width: 60, Height: 20})
	v := m.View()
	if strings.Contains(strings.Split(v, "\n")[1], "TIME") {
		t.Fatalf("TIME stays at 60 columns:\n%s", v)
	}
	cols := colStarts(t, v, "COST")
	for _, x := range strings.Split(v, "\n") {
		if r := []rune(x); strings.Contains(x, "$4.12") && !strings.HasPrefix(string(r[cols[0]:]), "$4.12") {
			t.Errorf("COST misaligned:\n%s", v)
		}
	}
}
