// Package herdr is the terminal.Backend for Herdr, driven over its local socket API
// in a dedicated session (named "rig" by default) so rig never touches the user's own
// Herdr workspaces. A ticket is a Herdr workspace and each service is a tab in it;
// the human attaches with `herdr session attach rig` (docs/spikes/02-herdr.md).
package herdr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

type Options struct {
	Session string // the Herdr session to use (default "rig")
	Binary  string // the herdr executable (default "herdr")
	// Socket skips discovery and uses this API socket path.
	Socket       string
	StartTimeout time.Duration // how long Ready waits for a server it starts (default 15s)
	PollInterval time.Duration // default 50ms
	Clock        clock.Clock   // nil means the real clock
	// ScriptDir is where launch scripts are written (default: the system temp dir).
	ScriptDir string
}

type Backend struct {
	opts Options

	mu     sync.Mutex // guards socket; spawnMu below serializes Spawn
	socket string
	seq    atomic.Int64

	spawnMu sync.Mutex // so two services never both create the ticket's workspace

	scriptsMu sync.Mutex
	scripts   map[terminal.PaneID]string // launch script directories of panes not yet seen to run them
}

var _ terminal.Backend = (*Backend)(nil)

func New(o Options) *Backend {
	if o.Session == "" {
		o.Session = "rig"
	}
	if o.Binary == "" {
		o.Binary = "herdr"
	}
	if o.StartTimeout == 0 {
		o.StartTimeout = 15 * time.Second
	}
	if o.PollInterval == 0 {
		o.PollInterval = 50 * time.Millisecond
	}
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	return &Backend{opts: o, socket: o.Socket}
}

// Ready makes sure the session's server is running, starting it headless and detached
// (so it outlives this process) if it is not.
func (b *Backend) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sweepLaunchScripts(b.opts.ScriptDir, b.opts.Clock.Now())
	socket, err := b.sock(ctx)
	if err != nil {
		return err
	}
	if b.ping(ctx, socket) == nil {
		return nil
	}

	binary, err := exec.LookPath(b.opts.Binary)
	if err != nil {
		return fmt.Errorf("%w: herdr is not installed or not on PATH (%v); install it from https://herdr.dev", terminal.ErrUnavailable, err)
	}
	cmd := exec.Command(binary, "--session", b.opts.Session, "server")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start the herdr server: %v", terminal.ErrUnavailable, err)
	}
	cmd.Process.Release()

	deadline := b.opts.Clock.After(b.opts.StartTimeout)
	for {
		if b.ping(ctx, socket) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("%w: the herdr server for session %q did not come up within %s", terminal.ErrUnavailable, b.opts.Session, b.opts.StartTimeout)
		case <-b.opts.Clock.After(b.opts.PollInterval):
		}
	}
}

func (b *Backend) ping(ctx context.Context, socket string) error {
	return b.call(ctx, socket, "ping", struct{}{}, nil)
}

// sock finds the API socket of the session: an explicit option, else what
// `herdr session list` says, else the conventional location beside the default session.
func (b *Backend) sock(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.socket != "" {
		return b.socket, nil
	}
	binary, err := exec.LookPath(b.opts.Binary)
	if err != nil {
		return "", fmt.Errorf("%w: herdr is not installed or not on PATH (%v); install it from https://herdr.dev", terminal.ErrUnavailable, err)
	}
	out, err := exec.CommandContext(ctx, binary, "session", "list").Output()
	if err != nil {
		return "", fmt.Errorf("%w: herdr session list: %v", terminal.ErrUnavailable, err)
	}
	var defaultSocket string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[0] {
		case b.opts.Session:
			b.socket = fields[len(fields)-1]
			return b.socket, nil
		case "default":
			defaultSocket = fields[len(fields)-1]
		}
	}
	if defaultSocket == "" {
		return "", fmt.Errorf("%w: cannot find herdr's socket directory in `herdr session list`", terminal.ErrUnavailable)
	}
	b.socket = filepath.Join(filepath.Dir(defaultSocket), "sessions", b.opts.Session, "herdr.sock")
	return b.socket, nil
}

type workspaceInfo struct {
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
}

type tabInfo struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

type paneInfo struct {
	ID          string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	AgentStatus string `json:"agent_status"`
}

type labels struct {
	workspaces map[string]string  // id -> ticket
	tabs       map[string]tabInfo // id -> service tab
}

func (b *Backend) labels(ctx context.Context, socket string) (labels, []workspaceInfo, error) {
	var ws struct {
		Workspaces []workspaceInfo `json:"workspaces"`
	}
	if err := b.call(ctx, socket, "workspace.list", struct{}{}, &ws); err != nil {
		return labels{}, nil, err
	}
	l := labels{workspaces: map[string]string{}, tabs: map[string]tabInfo{}}
	for _, w := range ws.Workspaces {
		l.workspaces[w.ID] = w.Label
		var tabs struct {
			Tabs []tabInfo `json:"tabs"`
		}
		if err := b.call(ctx, socket, "tab.list", map[string]string{"workspace_id": w.ID}, &tabs); err != nil {
			return labels{}, nil, err
		}
		for _, t := range tabs.Tabs {
			l.tabs[t.ID] = t
		}
	}
	return l, ws.Workspaces, nil
}

func (b *Backend) Spawn(ctx context.Context, spec terminal.SpawnSpec) (terminal.PaneID, error) {
	switch {
	case spec.Ticket == "" || spec.Service == "":
		return "", fmt.Errorf("%w: a ticket and a service are required", terminal.ErrInvalid)
	case !filepath.IsAbs(spec.Dir):
		return "", fmt.Errorf("%w: directory %q must be absolute", terminal.ErrInvalid, spec.Dir)
	}
	if info, err := os.Stat(spec.Dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: directory %s does not exist", terminal.ErrInvalid, spec.Dir)
	}
	line := ""
	if len(spec.Command) > 0 {
		var err error
		if line, err = shellLine(spec.Command, spec.Env); err != nil {
			return "", fmt.Errorf("%w: %v", terminal.ErrInvalid, err)
		}
	}
	if err := b.Ready(ctx); err != nil {
		return "", err
	}
	socket, err := b.sock(ctx)
	if err != nil {
		return "", err
	}

	b.spawnMu.Lock()
	defer b.spawnMu.Unlock()
	l, workspaces, err := b.labels(ctx, socket)
	if err != nil {
		return "", err
	}
	workspaceID := ""
	for _, w := range workspaces {
		if w.Label == spec.Ticket {
			workspaceID = w.ID
		}
	}
	for _, t := range l.tabs {
		if t.WorkspaceID == workspaceID && workspaceID != "" && t.Label == spec.Service {
			return "", fmt.Errorf("%w: %s/%s", terminal.ErrExists, spec.Ticket, spec.Service)
		}
	}

	var created struct {
		Tab      tabInfo  `json:"tab"`
		RootPane paneInfo `json:"root_pane"`
	}
	if workspaceID == "" {
		// A new workspace starts with one tab; the first service takes it over.
		if err := b.call(ctx, socket, "workspace.create", map[string]any{"cwd": spec.Dir, "label": spec.Ticket, "focus": false}, &created); err != nil {
			return "", err
		}
		if err := b.call(ctx, socket, "tab.rename", map[string]string{"tab_id": created.Tab.ID, "label": spec.Service}, nil); err != nil {
			b.call(context.WithoutCancel(ctx), socket, "pane.close", map[string]string{"pane_id": created.RootPane.ID}, nil)
			return "", err
		}
	} else if err := b.call(ctx, socket, "tab.create", map[string]any{"workspace_id": workspaceID, "cwd": spec.Dir, "label": spec.Service, "focus": false}, &created); err != nil {
		return "", err
	}

	pane := terminal.PaneID(created.RootPane.ID)
	if line != "" {
		typed, dir, err := writeLaunchScript(b.opts.ScriptDir, line)
		if err == nil {
			err = b.call(ctx, socket, "pane.send_input", map[string]any{"pane_id": created.RootPane.ID, "text": typed, "keys": []string{"Enter"}}, nil)
		}
		if err != nil {
			os.RemoveAll(dir)
			b.call(context.WithoutCancel(ctx), socket, "pane.close", map[string]string{"pane_id": created.RootPane.ID}, nil)
			return "", err
		}
		b.scriptsMu.Lock()
		if b.scripts == nil {
			b.scripts = map[terminal.PaneID]string{}
		}
		b.scripts[pane] = dir
		b.scriptsMu.Unlock()
	}
	return pane, nil
}

func (b *Backend) List(ctx context.Context) ([]terminal.Pane, error) {
	socket, err := b.sock(ctx)
	if err != nil {
		return nil, err
	}
	l, _, err := b.labels(ctx, socket)
	if err != nil {
		return nil, err
	}
	var res struct {
		Panes []paneInfo `json:"panes"`
	}
	if err := b.call(ctx, socket, "pane.list", struct{}{}, &res); err != nil {
		return nil, err
	}
	panes := make([]terminal.Pane, 0, len(res.Panes))
	for _, p := range res.Panes {
		panes = append(panes, toPane(p, l))
	}
	return panes, nil
}

func (b *Backend) Get(ctx context.Context, id terminal.PaneID) (terminal.Pane, error) {
	socket, err := b.sock(ctx)
	if err != nil {
		return terminal.Pane{}, err
	}
	var res struct {
		Pane paneInfo `json:"pane"`
	}
	if err := b.call(ctx, socket, "pane.get", map[string]string{"pane_id": string(id)}, &res); err != nil {
		return terminal.Pane{}, err
	}
	l, _, err := b.labels(ctx, socket)
	if err != nil {
		return terminal.Pane{}, err
	}
	return toPane(res.Pane, l), nil
}

func toPane(p paneInfo, l labels) terminal.Pane {
	return terminal.Pane{
		ID:        terminal.PaneID(p.ID),
		Ticket:    l.workspaces[p.WorkspaceID],
		Service:   l.tabs[p.TabID].Label,
		Alive:     true, // Herdr drops a pane when its process ends
		Heuristic: heuristic(p.AgentStatus),
	}
}

// heuristic maps Herdr's detected agent status. "done" means finished and not yet
// looked at, which for rig is idle.
func heuristic(status string) domain.AgentState {
	switch status {
	case "working":
		return domain.AgentWorking
	case "blocked":
		return domain.AgentBlocked
	case "idle", "done":
		return domain.AgentIdle
	}
	return domain.AgentUnknown
}

func (b *Backend) Read(ctx context.Context, id terminal.PaneID, lines int) (string, error) {
	socket, err := b.sock(ctx)
	if err != nil {
		return "", err
	}
	params := map[string]any{"pane_id": string(id), "source": "visible"}
	if lines > 0 {
		params["source"], params["lines"] = "recent", lines
	}
	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := b.call(ctx, socket, "pane.read", params, &res); err != nil {
		return "", err
	}
	return res.Read.Text, nil
}

func (b *Backend) SendText(ctx context.Context, id terminal.PaneID, text string) error {
	return b.simple(ctx, "pane.send_text", map[string]string{"pane_id": string(id), "text": text})
}

func (b *Backend) Submit(ctx context.Context, id terminal.PaneID) error {
	return b.simple(ctx, "pane.send_keys", map[string]any{"pane_id": string(id), "keys": []string{"Enter"}})
}

func (b *Backend) Close(ctx context.Context, id terminal.PaneID) error {
	err := b.simple(ctx, "pane.close", map[string]string{"pane_id": string(id)})
	// The script deletes itself when it runs; a pane closed before that would leave it.
	b.scriptsMu.Lock()
	dir := b.scripts[id]
	delete(b.scripts, id)
	b.scriptsMu.Unlock()
	if dir != "" {
		os.RemoveAll(dir)
	}
	return err
}

func (b *Backend) simple(ctx context.Context, method string, params any) error {
	socket, err := b.sock(ctx)
	if err != nil {
		return err
	}
	return b.call(ctx, socket, method, params, nil)
}

// Attach focuses the pane's tab in the session and returns the command that opens it.
func (b *Backend) Attach(ctx context.Context, id terminal.PaneID) ([]string, error) {
	socket, err := b.sock(ctx)
	if err != nil {
		return nil, err
	}
	var res struct {
		Pane paneInfo `json:"pane"`
	}
	if err := b.call(ctx, socket, "pane.get", map[string]string{"pane_id": string(id)}, &res); err != nil {
		return nil, err
	}
	// Focusing is a convenience for the human who attaches; failing to do it is not an error.
	b.call(ctx, socket, "workspace.focus", map[string]string{"workspace_id": res.Pane.WorkspaceID}, nil)
	b.call(ctx, socket, "tab.focus", map[string]string{"tab_id": res.Pane.TabID}, nil)
	return []string{b.opts.Binary, "session", "attach", b.opts.Session}, nil
}
