package tui

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"
)

// VesselRow is one vessel in the picker.
type VesselRow struct {
	Name, Status, Tickets string
}

// Picker chooses a vessel: j/k to move, / to search by name, enter to open.
type Picker struct {
	rows          []VesselRow
	sel           int
	filter        []rune
	typing        bool
	chosen        string
	width, height int
}

func NewPicker(rows []VesselRow) Picker { return Picker{rows: rows} }

// PickVessel shows the picker until a vessel is chosen ("" when the person quit).
func PickVessel(ctx context.Context, rows []VesselRow) (string, error) {
	final, err := tea.NewProgram(NewPicker(rows), tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if err != nil {
		return "", err
	}
	return final.(Picker).Chosen(), nil
}

// Chosen is the vessel picked, or "" when none was.
func (p Picker) Chosen() string { return p.chosen }

func (p Picker) Init() tea.Cmd { return nil }

func (p Picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return p.send(msg) }

func (p Picker) visible() []VesselRow {
	if len(p.filter) == 0 {
		return p.rows
	}
	want := strings.ToLower(string(p.filter))
	var out []VesselRow
	for _, r := range p.rows {
		if strings.Contains(strings.ToLower(r.Name), want) {
			out = append(out, r)
		}
	}
	return out
}

func (p Picker) send(msg tea.Msg) (Picker, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
	case tea.KeyMsg:
		return p.key(msg)
	}
	return p, nil
}

func (p Picker) key(msg tea.KeyMsg) (Picker, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return p, tea.Quit
	}
	if k == "enter" {
		rows := p.visible()
		if len(rows) == 0 {
			return p, nil
		}
		p.chosen = rows[min(p.sel, len(rows)-1)].Name
		return p, tea.Quit
	}
	if p.typing {
		switch msg.Type {
		case tea.KeyEsc:
			p.filter, p.typing = nil, false
		case tea.KeyBackspace:
			if len(p.filter) > 0 {
				p.filter = p.filter[:len(p.filter)-1]
			}
		case tea.KeyRunes:
			p.filter = append(p.filter, msg.Runes...)
		}
		p.sel = 0
		return p, nil
	}
	last := len(p.visible()) - 1
	switch k {
	case "q", "esc":
		return p, tea.Quit
	case "/":
		p.typing = true
	case "down", "j":
		p.sel = min(p.sel+1, max(last, 0))
	case "up", "k":
		p.sel = max(p.sel-1, 0)
	case "home", "g":
		p.sel = 0
	case "end", "G":
		p.sel = max(last, 0)
	}
	return p, nil
}

func (p Picker) View() string {
	w, h := p.width, p.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  VESSEL\tDAEMON\tTICKETS")
	rows := p.visible()
	for i, r := range rows {
		mark := " "
		if i == min(p.sel, len(rows)-1) {
			mark = ">"
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\n", mark, r.Name, r.Status, r.Tickets)
	}
	tw.Flush()
	body := b.String()
	if len(rows) == 0 {
		body += "  no vessel matches\n"
	}
	footer := "j/k move · / search · enter open · q quit"
	if p.typing {
		footer = "/" + string(p.filter) + "█   enter open · esc clear"
	}
	return strings.Join([]string{"rig · fleet · choose a vessel", fit(body, w, h-2), footer}, "\n")
}
