package herdr_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeHerdr speaks the subset of Herdr's socket API the backend uses, in memory, with
// the reply shapes of the real server (probed against Herdr 0.9.5). It runs no
// processes: a pane shows what is typed into it.
type fakeHerdr struct {
	t  *testing.T
	ln net.Listener

	mu         sync.Mutex
	path       string
	workspaces []*fakeWorkspace
	nextW      int
	nextT      int
	nextP      int
	panes      map[string]*fakePane
	calls      []string
	scripts    string // where the backend under test writes launch scripts
	failInput  bool   // pane.send_input fails, as when the shell is not ready
}

type fakeWorkspace struct {
	id, label string
	tabs      []*fakeTab
}

type fakeTab struct {
	id, label string
	ws        *fakeWorkspace
	panes     []*fakePane
}

type fakePane struct {
	id, cwd string
	tab     *fakeTab
	screen  strings.Builder
	typed   []string // texts and "<Enter>" markers, in order
	status  string
}

// shortSocketPath is a socket path under macOS's length limit.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

func startFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	return startFakeHerdrAt(t, shortSocketPath(t))
}

func startFakeHerdrAt(t *testing.T, path string) *fakeHerdr {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdr{t: t, ln: ln, path: path, panes: map[string]*fakePane{}}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeHerdr) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

type request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (f *fakeHerdr) handle(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, req.Method)
	result, apiErr := f.dispatch(req.Method, req.Params)
	f.mu.Unlock()

	reply := map[string]any{"id": req.ID}
	if apiErr != nil {
		reply["error"] = apiErr
	} else {
		reply["result"] = result
	}
	out, _ := json.Marshal(reply)
	conn.Write(append(out, '\n'))
}

type fakeErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (f *fakeHerdr) dispatch(method string, raw json.RawMessage) (any, *fakeErr) {
	var p struct {
		PaneID      string   `json:"pane_id"`
		TabID       string   `json:"tab_id"`
		WorkspaceID string   `json:"workspace_id"`
		Label       string   `json:"label"`
		Cwd         string   `json:"cwd"`
		Text        string   `json:"text"`
		Keys        []string `json:"keys"`
		Source      string   `json:"source"`
		Lines       int      `json:"lines"`
	}
	json.Unmarshal(raw, &p)

	switch method {
	case "ping":
		return map[string]any{"type": "pong", "version": "fake"}, nil
	case "workspace.list":
		list := []any{}
		for _, w := range f.workspaces {
			list = append(list, f.wsView(w))
		}
		return map[string]any{"type": "workspace_list", "workspaces": list}, nil
	case "tab.list":
		w := f.workspace(p.WorkspaceID)
		if w == nil {
			return nil, &fakeErr{"workspace_not_found", "workspace " + p.WorkspaceID + " not found"}
		}
		list := []any{}
		for _, t := range w.tabs {
			list = append(list, f.tabView(t))
		}
		return map[string]any{"type": "tab_list", "tabs": list}, nil
	case "pane.list":
		list := []any{}
		for _, w := range f.workspaces {
			for _, t := range w.tabs {
				for _, pn := range t.panes {
					list = append(list, f.paneView(pn))
				}
			}
		}
		return map[string]any{"type": "pane_list", "panes": list}, nil
	case "workspace.create":
		f.nextW++
		w := &fakeWorkspace{id: fmt.Sprintf("w%d", f.nextW), label: p.Label}
		f.workspaces = append(f.workspaces, w)
		t := f.newTab(w, "1")
		pn := f.newPane(t, p.Cwd)
		return map[string]any{"type": "workspace_created", "workspace": f.wsView(w), "tab": f.tabView(t), "root_pane": f.paneView(pn)}, nil
	case "tab.create":
		w := f.workspace(p.WorkspaceID)
		if w == nil {
			return nil, &fakeErr{"workspace_not_found", "workspace " + p.WorkspaceID + " not found"}
		}
		t := f.newTab(w, p.Label)
		pn := f.newPane(t, p.Cwd)
		return map[string]any{"type": "tab_created", "tab": f.tabView(t), "root_pane": f.paneView(pn)}, nil
	case "tab.rename":
		t := f.tab(p.TabID)
		if t == nil {
			return nil, &fakeErr{"tab_not_found", "tab " + p.TabID + " not found"}
		}
		t.label = p.Label
		return map[string]any{"type": "tab_info", "tab": f.tabView(t)}, nil
	case "workspace.focus", "tab.focus":
		return map[string]any{"type": "ok"}, nil
	}

	pn := f.panes[p.PaneID]
	if pn == nil {
		return nil, &fakeErr{"pane_not_found", "pane " + p.PaneID + " not found"}
	}
	switch method {
	case "pane.get":
		return map[string]any{"type": "pane_info", "pane": f.paneView(pn)}, nil
	case "pane.read":
		text := pn.screen.String()
		if p.Source == "recent" && p.Lines > 0 {
			lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			if len(lines) > p.Lines {
				lines = lines[len(lines)-p.Lines:]
			}
			text = strings.Join(lines, "\n") + "\n"
		}
		return map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": pn.id, "text": text, "source": p.Source}}, nil
	case "pane.send_text":
		pn.typed = append(pn.typed, p.Text)
		pn.screen.WriteString(p.Text)
	case "pane.send_keys":
		for _, k := range p.Keys {
			pn.typed = append(pn.typed, "<"+k+">")
			if k == "Enter" {
				pn.screen.WriteString("\n")
			}
		}
	case "pane.send_input":
		if f.failInput {
			return nil, &fakeErr{"pane_send_failed", "terminal is in canonical mode"}
		}
		pn.typed = append(pn.typed, p.Text)
		pn.screen.WriteString(p.Text)
		for _, k := range p.Keys {
			pn.typed = append(pn.typed, "<"+k+">")
			if k == "Enter" {
				pn.screen.WriteString("\n")
			}
		}
	case "pane.close":
		f.removePane(pn)
	default:
		return nil, &fakeErr{"invalid_request", "unknown variant `" + method + "`"}
	}
	return map[string]any{"type": "ok"}, nil
}

func (f *fakeHerdr) newTab(w *fakeWorkspace, label string) *fakeTab {
	f.nextT++
	t := &fakeTab{id: fmt.Sprintf("%s:t%d", w.id, f.nextT), label: label, ws: w}
	w.tabs = append(w.tabs, t)
	return t
}

func (f *fakeHerdr) newPane(t *fakeTab, cwd string) *fakePane {
	f.nextP++
	pn := &fakePane{id: fmt.Sprintf("%s:p%d", t.ws.id, f.nextP), cwd: cwd, tab: t, status: "unknown"}
	t.panes = append(t.panes, pn)
	f.panes[pn.id] = pn
	return pn
}

// removePane mirrors Herdr: closing a tab's last pane removes the tab, and a
// workspace's last tab removes the workspace.
func (f *fakeHerdr) removePane(pn *fakePane) {
	delete(f.panes, pn.id)
	t := pn.tab
	for i, other := range t.panes {
		if other == pn {
			t.panes = append(t.panes[:i], t.panes[i+1:]...)
			break
		}
	}
	if len(t.panes) > 0 {
		return
	}
	w := t.ws
	for i, other := range w.tabs {
		if other == t {
			w.tabs = append(w.tabs[:i], w.tabs[i+1:]...)
			break
		}
	}
	if len(w.tabs) > 0 {
		return
	}
	for i, other := range f.workspaces {
		if other == w {
			f.workspaces = append(f.workspaces[:i], f.workspaces[i+1:]...)
			return
		}
	}
}

func (f *fakeHerdr) workspace(id string) *fakeWorkspace {
	for _, w := range f.workspaces {
		if w.id == id {
			return w
		}
	}
	return nil
}

func (f *fakeHerdr) tab(id string) *fakeTab {
	for _, w := range f.workspaces {
		for _, t := range w.tabs {
			if t.id == id {
				return t
			}
		}
	}
	return nil
}

func (f *fakeHerdr) wsView(w *fakeWorkspace) map[string]any {
	return map[string]any{"workspace_id": w.id, "label": w.label, "tab_count": len(w.tabs)}
}

func (f *fakeHerdr) tabView(t *fakeTab) map[string]any {
	return map[string]any{"tab_id": t.id, "workspace_id": t.ws.id, "label": t.label, "pane_count": len(t.panes)}
}

func (f *fakeHerdr) paneView(pn *fakePane) map[string]any {
	return map[string]any{"pane_id": pn.id, "workspace_id": pn.tab.ws.id, "tab_id": pn.tab.id, "cwd": pn.cwd, "agent_status": pn.status}
}

// Test hooks.

// exitPane ends the pane's process the way Herdr shows it: the pane disappears.
func (f *fakeHerdr) exitPane(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pn := f.panes[id]; pn != nil {
		f.removePane(pn)
	}
}

func (f *fakeHerdr) setStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pn := f.panes[id]; pn != nil {
		pn.status = status
	}
}

func (f *fakeHerdr) typed(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pn := f.panes[id]; pn != nil {
		return append([]string(nil), pn.typed...)
	}
	return nil
}

func (f *fakeHerdr) snapshot() (workspaces map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	workspaces = map[string][]string{}
	for _, w := range f.workspaces {
		for _, t := range w.tabs {
			workspaces[w.label] = append(workspaces[w.label], t.label)
		}
		if len(w.tabs) == 0 {
			workspaces[w.label] = nil
		}
	}
	return workspaces
}

func (f *fakeHerdr) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *fakeHerdr) stop() { f.ln.Close(); os.Remove(f.path) }

func (f *fakeHerdr) failInputs() { f.mu.Lock(); f.failInput = true; f.mu.Unlock() }
