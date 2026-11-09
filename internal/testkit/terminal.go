package testkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

// FakeTerminal is an in-memory terminal.Backend. It runs no processes: a pane shows
// what was typed into it and what a test prints to it, and a test scripts the rest
// (the process exiting, the pane vanishing, the screen suggesting a state, the backend
// going down). It behaves like tmux for a process that ends (the pane stays listed
// until closed); Vanish behaves like Herdr dropping it.
type FakeTerminal struct {
	mu      sync.Mutex
	next    int
	order   []terminal.PaneID
	panes   map[terminal.PaneID]*fakePane
	spawned []terminal.SpawnSpec
	down    error
}

var _ terminal.Backend = (*FakeTerminal)(nil)

// Input is one thing typed into a pane: text, or the Enter that submits it.
type Input struct {
	Text   string
	Submit bool
}

type fakePane struct {
	spec      terminal.SpawnSpec
	alive     bool
	exit      *int
	heuristic domain.AgentState
	screen    strings.Builder
	input     []Input
}

func NewFakeTerminal() *FakeTerminal {
	return &FakeTerminal{panes: map[terminal.PaneID]*fakePane{}}
}

// Down makes every operation fail as if the backend were unreachable; Up undoes it.
func (f *FakeTerminal) Down(cause error) { f.mu.Lock(); f.down = cause; f.mu.Unlock() }
func (f *FakeTerminal) Up()              { f.Down(nil) }

// Exit ends the pane's process with a status. The pane stays listed, not alive.
func (f *FakeTerminal) Exit(id terminal.PaneID, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.panes[id]; p != nil {
		p.alive = false
		p.exit = &status
	}
}

// Vanish drops the pane entirely, as Herdr does when its process ends.
func (f *FakeTerminal) Vanish(id terminal.PaneID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remove(id)
}

func (f *FakeTerminal) SetHeuristic(id terminal.PaneID, state domain.AgentState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.panes[id]; p != nil {
		p.heuristic = state
	}
}

// Print shows text on the pane's screen, as if the process had written it.
func (f *FakeTerminal) Print(id terminal.PaneID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.panes[id]; p != nil {
		p.screen.WriteString(text)
	}
}

// Input returns what was typed into the pane, in order.
func (f *FakeTerminal) Input(id terminal.PaneID) []Input {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.panes[id]; p != nil {
		return append([]Input(nil), p.input...)
	}
	return nil
}

// Spawned returns every spec passed to a successful Spawn, in order.
func (f *FakeTerminal) Spawned() []terminal.SpawnSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]terminal.SpawnSpec(nil), f.spawned...)
}

func (f *FakeTerminal) unavailable() error {
	if f.down != nil {
		return fmt.Errorf("%w: %v", terminal.ErrUnavailable, f.down)
	}
	return nil
}

func (f *FakeTerminal) Ready(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.unavailable()
}

func (f *FakeTerminal) Spawn(ctx context.Context, spec terminal.SpawnSpec) (terminal.PaneID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := f.unavailable(); err != nil {
		return "", err
	}
	switch {
	case spec.Ticket == "" || spec.Service == "":
		return "", fmt.Errorf("%w: a ticket and a service are required", terminal.ErrInvalid)
	case !filepath.IsAbs(spec.Dir):
		return "", fmt.Errorf("%w: directory %q must be absolute", terminal.ErrInvalid, spec.Dir)
	}
	if info, err := os.Stat(spec.Dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: directory %s does not exist", terminal.ErrInvalid, spec.Dir)
	}
	for _, id := range f.order {
		if p := f.panes[id].spec; p.Ticket == spec.Ticket && p.Service == spec.Service {
			return "", fmt.Errorf("%w: %s/%s", terminal.ErrExists, spec.Ticket, spec.Service)
		}
	}
	f.next++
	id := terminal.PaneID(fmt.Sprintf("fake-%d", f.next))
	f.panes[id] = &fakePane{spec: spec, alive: true, heuristic: domain.AgentUnknown}
	f.order = append(f.order, id)
	f.spawned = append(f.spawned, spec)
	return id, nil
}

func (f *FakeTerminal) List(ctx context.Context) ([]terminal.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.unavailable(); err != nil {
		return nil, err
	}
	panes := make([]terminal.Pane, 0, len(f.order))
	for _, id := range f.order {
		panes = append(panes, f.view(id))
	}
	return panes, nil
}

func (f *FakeTerminal) Get(ctx context.Context, id terminal.PaneID) (terminal.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.pane(ctx, id); err != nil {
		return terminal.Pane{}, err
	}
	return f.view(id), nil
}

func (f *FakeTerminal) Read(ctx context.Context, id terminal.PaneID, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.pane(ctx, id)
	if err != nil {
		return "", err
	}
	text := p.screen.String()
	if lines > 0 {
		all := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		if len(all) > lines {
			all = all[len(all)-lines:]
		}
		text = strings.Join(all, "\n")
	}
	return text, nil
}

func (f *FakeTerminal) SendText(ctx context.Context, id terminal.PaneID, text string) error {
	return f.type_(ctx, id, Input{Text: text}, text)
}

func (f *FakeTerminal) Submit(ctx context.Context, id terminal.PaneID) error {
	return f.type_(ctx, id, Input{Submit: true}, "\n")
}

func (f *FakeTerminal) type_(ctx context.Context, id terminal.PaneID, in Input, echo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.pane(ctx, id)
	if err != nil {
		return err
	}
	if !p.alive {
		return fmt.Errorf("%w: the process in %s has exited", terminal.ErrNotFound, id)
	}
	p.input = append(p.input, in)
	p.screen.WriteString(echo)
	return nil
}

func (f *FakeTerminal) Close(ctx context.Context, id terminal.PaneID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.pane(ctx, id); err != nil {
		return err
	}
	f.remove(id)
	return nil
}

func (f *FakeTerminal) Attach(ctx context.Context, id terminal.PaneID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.pane(ctx, id); err != nil {
		return nil, err
	}
	return []string{"fake-attach", string(id)}, nil
}

// pane looks a pane up, after the context and availability checks. f.mu must be held.
func (f *FakeTerminal) pane(ctx context.Context, id terminal.PaneID) (*fakePane, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.unavailable(); err != nil {
		return nil, err
	}
	p := f.panes[id]
	if p == nil {
		return nil, fmt.Errorf("%w: %s", terminal.ErrNotFound, id)
	}
	return p, nil
}

func (f *FakeTerminal) view(id terminal.PaneID) terminal.Pane {
	p := f.panes[id]
	out := terminal.Pane{ID: id, Ticket: p.spec.Ticket, Service: p.spec.Service, Alive: p.alive, Heuristic: p.heuristic}
	if p.exit != nil {
		status := *p.exit
		out.ExitStatus = &status
	}
	return out
}

func (f *FakeTerminal) remove(id terminal.PaneID) {
	delete(f.panes, id)
	for i, other := range f.order {
		if other == id {
			f.order = append(f.order[:i], f.order[i+1:]...)
			return
		}
	}
}
