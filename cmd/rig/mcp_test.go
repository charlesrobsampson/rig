package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/testkit"
)

// mcpClient drives `rig mcp` as a supervising agent would: JSON-RPC lines over its
// standard input and output.
type mcpClient struct {
	t     *testing.T
	in    io.WriteCloser
	lines *bufio.Scanner
	done  chan int
	id    int
}

func startMCP(t *testing.T, d deps) *mcpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	d.stdin = inR
	done := make(chan int, 1)
	go func() {
		code := run(context.Background(), []string{"mcp"}, outW, io.Discard, d)
		outW.Close()
		done <- code
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	c := &mcpClient{t: t, in: inW, lines: sc, done: done}
	t.Cleanup(func() { inW.Close() })
	return c
}

func (c *mcpClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	raw, _ := json.Marshal(params)
	fmt.Fprintf(c.in, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", c.id, method, raw)
	got := make(chan string, 1)
	go func() {
		if c.lines.Scan() {
			got <- c.lines.Text()
		} else {
			got <- ""
		}
	}()
	select {
	case line := <-got:
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			c.t.Fatalf("not JSON on stdout: %q", line)
		}
		return m
	case <-time.After(10 * time.Second):
		c.t.Fatal("no answer from rig mcp")
		return nil
	}
}

func (c *mcpClient) tool(name string, args any) (string, bool) {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	res := resp["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func TestRigMCPSupervisesRealTicketsThroughTheDaemon(t *testing.T) {
	home, d, repo, term := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo, testkit.NewRepo(t))
	os.WriteFile(filepath.Join(home, "tickets", "demo", "plan.md"), []byte("# The plan\n"), 0o600)
	rig(t, d, "start", "demo", "--sandbox", "host")
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", "idle")

	c := startMCP(t, d)
	if resp := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18"}); resp["error"] != nil {
		t.Fatalf("initialize: %v", resp)
	}
	if tools := c.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != 6 {
		t.Errorf("%d tools", len(tools))
	}

	text, isErr := c.tool("rig_list_tickets", map[string]any{})
	if isErr || !strings.Contains(text, `"id": "demo"`) || !strings.Contains(text, `"name": "svc1"`) {
		t.Errorf("list: %q", text)
	}
	text, isErr = c.tool("rig_get_ticket", map[string]any{"ticket": "demo"})
	if isErr || !strings.Contains(text, `"phase": "plan"`) || !strings.Contains(text, "plan_approval") {
		t.Errorf("get: %q", text)
	}
	if text, isErr = c.tool("rig_read_artifact", map[string]any{"ticket": "demo", "name": "plan.md"}); isErr || text != "# The plan\n" {
		t.Errorf("artifact: %q", text)
	}
	if text, isErr = c.tool("rig_read_artifact", map[string]any{"ticket": "demo", "name": "state.json"}); !isErr || !strings.Contains(text, "invalid") {
		t.Errorf("state.json must not be readable: %q %v", text, isErr)
	}

	if text, isErr = c.tool("rig_send_message", map[string]any{"ticket": "demo", "service": "svc1", "text": "please rerun the tests"}); isErr || text != "sent to demo/svc1" {
		t.Errorf("message: %q", text)
	}
	panes, _ := term.List(context.Background())
	if in := term.Input(panes[0].ID); len(in) == 0 || in[0].Text != "[rig] please rerun the tests" {
		t.Errorf("typed %v", in)
	}

	// a transition the gates do not allow comes back as a tool error that lists what is missing
	text, isErr = c.tool("rig_request_transition", map[string]any{"ticket": "demo", "service": "svc1", "to": "implement"})
	if !isErr || !strings.Contains(text, "rig error (blocked)") || !strings.Contains(text, "plan_approval") {
		t.Errorf("transition: %q %v", text, isErr)
	}
	if out, _, _ := rig(t, d, "gates", "demo"); strings.Contains(out, "approved by") {
		t.Errorf("a request through MCP approved something:\n%s", out)
	}

	// proposing a contract change appends it; the agreed section stays untouched
	before, _ := os.ReadFile(filepath.Join(home, "tickets", "demo", "contract.md"))
	text, isErr = c.tool("rig_propose_contract_change", map[string]any{"ticket": "demo", "service": "svc1", "what": "add a field", "why": "needed"})
	if isErr || !strings.Contains(text, "proposed P-svc1-1") {
		t.Fatalf("propose: %q %v", text, isErr)
	}
	after, _ := os.ReadFile(filepath.Join(home, "tickets", "demo", "contract.md"))
	if !strings.Contains(string(after), "### P-svc1-1 · from svc1 · open") || !strings.Contains(string(after), "**What:** add a field") || len(after) <= len(before) {
		t.Errorf("contract.md:\n%s", after)
	}
	if out, _, _ := rig(t, d, "contract", "list", "demo"); !strings.Contains(out, "P-svc1-1  from svc1  [open]") {
		t.Errorf("the human cannot see the proposal:\n%s", out)
	}

	// hanging up ends the server cleanly
	c.in.Close()
	select {
	case code := <-c.done:
		if code != 0 {
			t.Errorf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rig mcp did not exit at end of input")
	}
}

func TestRigMCPPrintsNothingToStdoutButProtocol(t *testing.T) {
	_, d, _ := newEnv(t)
	c := startMCP(t, d)
	for i := 0; i < 3; i++ {
		if resp := c.call("ping", nil); resp["jsonrpc"] != "2.0" {
			t.Errorf("not a protocol message: %v", resp)
		}
	}
	// an unreachable daemon is a tool error, not a crash
	if text, isErr := c.tool("rig_get_ticket", map[string]any{"ticket": "ghost"}); !isErr || !strings.Contains(text, "not_found") {
		t.Errorf("unknown ticket: %q %v", text, isErr)
	}
	if _, _, code := rig(t, d, "mcp", "extra"); code != 2 {
		t.Errorf("stray argument: code %d", code)
	}
}
