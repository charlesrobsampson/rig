package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/mcp"
)

// fakeDaemon records every call and answers from a table of canned results.
type fakeDaemon struct {
	mu      sync.Mutex
	calls   []call
	results map[string]any   // method -> result to decode into the caller's struct
	errs    map[string]error // method -> error
	block   map[string]chan struct{}
	panics  map[string]bool
}

type call struct {
	Method string
	Params any
}

func (f *fakeDaemon) Call(ctx context.Context, method string, params, result any) error {
	f.mu.Lock()
	f.calls = append(f.calls, call{method, params})
	err, res, block, doPanic := f.errs[method], f.results[method], f.block[method], f.panics[method]
	f.mu.Unlock()
	if doPanic {
		panic("boom")
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	if res != nil && result != nil {
		raw, _ := json.Marshal(res)
		return json.Unmarshal(raw, result)
	}
	return nil
}

func (f *fakeDaemon) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

// session is a client talking to a server over pipes.
type session struct {
	t      *testing.T
	in     io.WriteCloser
	lines  chan string
	done   chan error
	cancel context.CancelFunc
	srv    *mcp.Server
}

func newSession(t *testing.T, d *fakeDaemon) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := mcp.NewServer(mcp.Options{Daemon: d, Version: "v-test"})
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{t: t, in: inW, lines: make(chan string, 100), done: make(chan error, 1), cancel: cancel, srv: srv}
	go func() { s.done <- srv.Serve(ctx, inR, outW); outW.Close() }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
	}()
	t.Cleanup(func() { cancel(); inW.Close() })
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) recv() map[string]any {
	s.t.Helper()
	select {
	case line, ok := <-s.lines:
		if !ok {
			s.t.Fatal("the server closed its output")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			s.t.Fatalf("the server wrote something that is not JSON: %q", line)
		}
		if m["jsonrpc"] != "2.0" {
			s.t.Fatalf("not a JSON-RPC 2.0 message: %q", line)
		}
		return m
	case <-time.After(3 * time.Second):
		s.t.Fatal("no message from the server")
		return nil
	}
}

func (s *session) silent() {
	s.t.Helper()
	select {
	case line := <-s.lines:
		s.t.Fatalf("the server wrote %q but should have stayed silent", line)
	case <-time.After(100 * time.Millisecond):
	}
}

func (s *session) rpc(id int, method string, params any) map[string]any {
	s.t.Helper()
	raw, _ := json.Marshal(params)
	s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, raw))
	return s.recv()
}

func (s *session) callTool(id int, name string, args any) map[string]any {
	s.t.Helper()
	return s.rpc(id, "tools/call", map[string]any{"name": name, "arguments": args})
}

// toolText returns a tool result's text and whether it is an error.
func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	if resp["error"] != nil {
		t.Fatalf("a protocol error where a tool result was expected: %v", resp["error"])
	}
	res := resp["result"].(map[string]any)
	content := res["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content = %v", content)
	}
	isErr, _ := res["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isErr
}

func rpcErrorCode(t *testing.T, resp map[string]any) int {
	t.Helper()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error in %v", resp)
	}
	return int(e["code"].(float64))
}

func TestInitializeNegotiatesTheProtocolAndSaysWhoItIs(t *testing.T) {
	s := newSession(t, &fakeDaemon{})
	resp := s.rpc(1, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})
	res := resp["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["name"] != "rig" || res["serverInfo"].(map[string]any)["version"] != "v-test" {
		t.Errorf("result = %v", res)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities = %v", res["capabilities"])
	}
	if ins := res["instructions"].(string); !strings.Contains(ins, "human") || !strings.Contains(ins, "never through this server") {
		t.Errorf("instructions = %q", ins)
	}
	// a revision it does not know gets the latest it does
	if r := s.rpc(2, "initialize", map[string]any{"protocolVersion": "1999-01-01"}); r["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Errorf("unknown revision: %v", r["result"])
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.silent()
}

func TestPingAndUnknownMethods(t *testing.T) {
	s := newSession(t, &fakeDaemon{})
	if r := s.rpc(1, "ping", nil); r["error"] != nil || r["id"] != float64(1) {
		t.Errorf("ping = %v", r)
	}
	if code := rpcErrorCode(t, s.rpc(2, "resources/list", nil)); code != -32601 {
		t.Errorf("code = %d, want method not found", code)
	}
	s.send(`{"jsonrpc":"2.0","method":"something/new"}`) // an unknown notification is ignored
	s.silent()
}

func TestTheToolListIsExactlyTheDocumentedSet(t *testing.T) {
	s := newSession(t, &fakeDaemon{})
	resp := s.rpc(1, "tools/list", nil)
	var names []string
	for _, raw := range resp["result"].(map[string]any)["tools"].([]any) {
		tool := raw.(map[string]any)
		names = append(names, tool["name"].(string))
		schema := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("%s: schema = %v", tool["name"], schema)
		}
		if tool["description"].(string) == "" {
			t.Errorf("%s has no description", tool["name"])
		}
		if tool["annotations"] == nil {
			t.Errorf("%s has no annotations", tool["name"])
		}
	}
	if !reflect.DeepEqual(names, mcp.ToolNames) || !reflect.DeepEqual(names, []string{
		"rig_list_tickets", "rig_get_ticket", "rig_read_artifact", "rig_send_message", "rig_request_transition", "rig_propose_contract_change",
	}) {
		t.Errorf("tools = %v", names)
	}
	if got := len(s.srv.Tools()); got != 6 {
		t.Errorf("Tools() has %d", got)
	}
}

func TestNoToolCanApproveAnything(t *testing.T) {
	for _, name := range mcp.ToolNames {
		for _, banned := range []string{"approve", "approval", "gate", "signoff", "sign_off", "accept", "reject", "push", "stop", "phase_set", "force", "ack"} {
			if strings.Contains(name, banned) {
				t.Errorf("tool %q looks like it decides something (%q)", name, banned)
			}
		}
	}
	// and calling every tool never reaches a decision method
	d := &fakeDaemon{results: map[string]any{
		"ticket.list": api.TicketListResult{}, "ticket.get": api.TicketGetResult{}, "gate.list": api.GateListResult{},
		"ticket.file": api.TicketFileResult{Content: "x"}, "agent.message": api.AgentMessageResult{Service: "api"},
		"transition.request": api.TransitionResult{From: domain.PhasePlan, To: domain.PhaseImplement}, "contract.propose": api.ContractProposeResult{Proposal: "P-api-1"},
	}}
	s := newSession(t, d)
	for i, c := range []struct {
		name string
		args any
	}{
		{"rig_list_tickets", map[string]any{}},
		{"rig_get_ticket", map[string]any{"ticket": "t"}},
		{"rig_read_artifact", map[string]any{"ticket": "t", "name": "plan.md"}},
		{"rig_send_message", map[string]any{"ticket": "t", "service": "api", "text": "hi"}},
		{"rig_request_transition", map[string]any{"ticket": "t", "service": "api", "to": "implement"}},
		{"rig_propose_contract_change", map[string]any{"ticket": "t", "service": "api", "what": "w", "why": "y"}},
	} {
		if _, isErr := toolText(t, s.callTool(i+1, c.name, c.args)); isErr {
			t.Errorf("%s failed", c.name)
		}
	}
	allowed := map[string]bool{"ticket.list": true, "ticket.get": true, "gate.list": true, "ticket.file": true, "agent.message": true, "transition.request": true, "contract.propose": true}
	for _, m := range d.methods() {
		if !allowed[m] {
			t.Errorf("the MCP server called %s, which is not part of its documented surface", m)
		}
	}
	for _, forbidden := range []string{"gate.approve", "phase.set", "contract.accept", "contract.reject", "contract.ack", "tests.signoff", "tests.step", "push.run", "ticket.stop", "ticket.start", "daemon.stop", "ticket.new"} {
		for _, m := range d.methods() {
			if m == forbidden {
				t.Errorf("%s was called", forbidden)
			}
		}
	}
}

func TestEachToolMapsToTheRightAPICall(t *testing.T) {
	d := &fakeDaemon{results: map[string]any{
		"ticket.list":        api.TicketListResult{Tickets: []api.TicketSummary{{ID: "demo-1", Title: "Add locale", Phase: domain.PhaseImplement}}},
		"ticket.get":         api.TicketGetResult{Summary: api.TicketSummary{ID: "demo-1", Title: "Add locale"}},
		"gate.list":          api.GateListResult{Phase: domain.PhasePlan, Gates: []api.GateStatus{{Gate: domain.GatePlanApproval, From: domain.PhasePlan, To: domain.PhaseImplement, Service: "api"}}},
		"ticket.file":        api.TicketFileResult{Name: "plan.md", Content: "# Plan\nbody"},
		"agent.message":      api.AgentMessageResult{Service: "api", Sent: "[rig] hi"},
		"transition.request": api.TransitionResult{From: domain.PhasePlan, To: domain.PhaseImplement, By: "agent:api"},
		"contract.propose":   api.ContractProposeResult{Proposal: "P-api-3"},
	}}
	s := newSession(t, d)

	text, _ := toolText(t, s.callTool(1, "rig_list_tickets", map[string]any{}))
	if !strings.Contains(text, `"id": "demo-1"`) || !strings.Contains(text, `"phase": "implement"`) {
		t.Errorf("list = %s", text)
	}
	text, _ = toolText(t, s.callTool(2, "rig_get_ticket", map[string]any{"ticket": "demo-1"}))
	if !strings.Contains(text, `"summary"`) || !strings.Contains(text, `"gates"`) || !strings.Contains(text, "plan_approval") {
		t.Errorf("get = %s", text)
	}
	text, _ = toolText(t, s.callTool(3, "rig_read_artifact", map[string]any{"ticket": "demo-1", "name": "plan.md"}))
	if text != "# Plan\nbody" {
		t.Errorf("artifact = %q", text)
	}
	text, _ = toolText(t, s.callTool(4, "rig_send_message", map[string]any{"ticket": "demo-1", "service": "api", "text": "please rerun"}))
	if text != "sent to demo-1/api" {
		t.Errorf("message = %q", text)
	}
	text, _ = toolText(t, s.callTool(5, "rig_request_transition", map[string]any{"ticket": "demo-1", "service": "api", "to": "implement", "reason": "ready"}))
	if !strings.Contains(text, `"to": "implement"`) {
		t.Errorf("transition = %s", text)
	}
	text, _ = toolText(t, s.callTool(6, "rig_propose_contract_change", map[string]any{"ticket": "demo-1", "service": "api", "what": "add tz", "why": "clients"}))
	if !strings.Contains(text, "proposed P-api-3") || !strings.Contains(text, "human must accept") {
		t.Errorf("propose = %q", text)
	}

	want := []call{
		{"ticket.list", nil},
		{"ticket.get", api.TicketGetParams{ID: "demo-1"}},
		{"gate.list", api.GateListParams{ID: "demo-1"}},
		{"ticket.file", api.TicketFileParams{Ticket: "demo-1", Name: "plan.md"}},
		{"agent.message", api.AgentMessageParams{Ticket: "demo-1", Service: "api", Text: "please rerun"}},
		{"transition.request", api.TransitionRequestParams{ID: "demo-1", Service: "api", To: domain.PhaseImplement, Reason: "ready"}},
		{"contract.propose", api.ContractProposeParams{Ticket: "demo-1", Service: "api", What: "add tz", Why: "clients"}},
	}
	if !reflect.DeepEqual(d.calls, want) {
		t.Errorf("calls:\n got %+v\nwant %+v", d.calls, want)
	}
}

func TestDaemonErrorsBecomeToolErrorsNotCrashes(t *testing.T) {
	d := &fakeDaemon{errs: map[string]error{
		"transition.request": &api.Error{Code: api.CodeBlocked, Message: `ticket "demo" cannot move plan -> implement until plan_approval is approved`, Details: []string{"plan_approval: api"}},
		"agent.message":      &api.Error{Code: api.CodeBlocked, Message: "the agent is waiting on a permission prompt"},
		"ticket.file":        &api.Error{Code: api.CodeNotFound, Message: "ticket has no contract.md"},
		"ticket.list":        errors.New("dial unix /x/rig.sock: connect: no such file or directory\nis the daemon running?"),
	}, panics: map[string]bool{"contract.propose": true}}
	s := newSession(t, d)

	text, isErr := toolText(t, s.callTool(1, "rig_request_transition", map[string]any{"ticket": "demo", "service": "api", "to": "implement"}))
	if !isErr || !strings.Contains(text, "rig error (blocked)") || !strings.Contains(text, "- plan_approval: api") {
		t.Errorf("blocked transition: %q (error=%v)", text, isErr)
	}
	text, isErr = toolText(t, s.callTool(2, "rig_send_message", map[string]any{"ticket": "demo", "service": "api", "text": "x"}))
	if !isErr || !strings.Contains(text, "waiting on a permission prompt") {
		t.Errorf("blocked message: %q", text)
	}
	text, isErr = toolText(t, s.callTool(3, "rig_read_artifact", map[string]any{"ticket": "demo", "name": "contract.md"}))
	if !isErr || !strings.Contains(text, "rig error (not_found)") {
		t.Errorf("missing file: %q", text)
	}
	text, isErr = toolText(t, s.callTool(4, "rig_list_tickets", map[string]any{}))
	if !isErr || !strings.HasPrefix(text, "error: dial unix") || strings.Contains(text, "\n") {
		t.Errorf("unreachable daemon: %q", text)
	}
	text, isErr = toolText(t, s.callTool(5, "rig_propose_contract_change", map[string]any{"ticket": "demo", "service": "api", "what": "w", "why": "y"}))
	if !isErr || !strings.Contains(text, "internal error") {
		t.Errorf("a panic in a tool: %q", text)
	}
	// the server is still alive
	if r := s.rpc(6, "ping", nil); r["error"] != nil {
		t.Errorf("the server did not survive: %v", r)
	}
}

func TestBadArgumentsAreToolErrorsAndNothingIsSent(t *testing.T) {
	d := &fakeDaemon{}
	s := newSession(t, d)
	tests := []struct {
		name string
		tool string
		args any
		want string
	}{
		{"a missing required argument", "rig_get_ticket", map[string]any{}, `"ticket" is required`},
		{"a blank required argument", "rig_send_message", map[string]any{"ticket": "t", "service": "api", "text": "  "}, `"text" is required`},
		{"a wrongly typed argument", "rig_get_ticket", map[string]any{"ticket": 5}, "bad arguments"},
		{"an unexpected argument", "rig_read_artifact", map[string]any{"ticket": "t", "name": "plan.md", "path": "/etc/passwd"}, "unknown field"},
		{"arguments for a tool that takes none", "rig_list_tickets", map[string]any{"all": true}, "takes none"},
		{"a bad phase", "rig_request_transition", map[string]any{"ticket": "t", "service": "api", "to": "launch"}, "is not a phase"},
		{"no arguments at all", "rig_propose_contract_change", nil, "is required"},
	}
	for i, tt := range tests {
		text, isErr := toolText(t, s.callTool(i+1, tt.tool, tt.args))
		if !isErr || !strings.Contains(text, tt.want) {
			t.Errorf("%s: %q (error=%v), want %q", tt.name, text, isErr, tt.want)
		}
	}
	if len(d.calls) != 0 {
		t.Errorf("bad arguments still reached the daemon: %v", d.calls)
	}
}

func TestProtocolLevelProblemsAreProtocolErrors(t *testing.T) {
	s := newSession(t, &fakeDaemon{})
	if code := rpcErrorCode(t, s.callTool(1, "rig_delete_everything", map[string]any{})); code != -32602 {
		t.Errorf("unknown tool code = %d", code)
	}
	if code := rpcErrorCode(t, s.rpc(2, "tools/call", map[string]any{})); code != -32602 {
		t.Errorf("no name code = %d", code)
	}
	s.send(`this is not json`)
	if r := s.recv(); r["id"] != nil || int(r["error"].(map[string]any)["code"].(float64)) != -32700 {
		t.Errorf("parse error = %v", r)
	}
	s.send(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if code := rpcErrorCode(t, s.recv()); code != -32600 {
		t.Errorf("batch code = %d", code)
	}
	s.send(`{"jsonrpc":"1.0","id":7,"method":"ping"}`)
	if r := s.recv(); r["id"] != float64(7) || int(r["error"].(map[string]any)["code"].(float64)) != -32600 {
		t.Errorf("wrong version = %v", r)
	}
	// blank lines are ignored and the session carries on
	s.send("")
	s.send("   ")
	if r := s.rpc(8, "ping", nil); r["error"] != nil {
		t.Errorf("ping after garbage: %v", r)
	}
}

func TestAnOversizedMessageIsRefusedWithoutKillingTheSession(t *testing.T) {
	s := newSession(t, &fakeDaemon{})
	s.send(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", 5<<20) + `"}}`)
	if code := rpcErrorCode(t, s.recv()); code != -32600 {
		t.Errorf("code = %d", code)
	}
	if r := s.rpc(2, "ping", nil); r["error"] != nil {
		t.Errorf("ping after it: %v", r)
	}
}

func TestASlowToolDoesNotBlockOtherRequests(t *testing.T) {
	release := make(chan struct{})
	d := &fakeDaemon{block: map[string]chan struct{}{"agent.message": release}, results: map[string]any{"agent.message": api.AgentMessageResult{Service: "api"}}}
	s := newSession(t, d)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rig_send_message","arguments":{"ticket":"t","service":"api","text":"hi"}}}`)
	if r := s.rpc(2, "ping", nil); r["id"] != float64(2) {
		t.Fatalf("ping answered %v while the message was pending", r)
	}
	close(release)
	if r := s.recv(); r["id"] != float64(1) {
		t.Errorf("the slow call answered %v", r)
	}
}

func TestCancellingARequestStopsItsDaemonCall(t *testing.T) {
	d := &fakeDaemon{block: map[string]chan struct{}{"agent.message": make(chan struct{})}}
	s := newSession(t, d)
	s.send(`{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"rig_send_message","arguments":{"ticket":"t","service":"api","text":"hi"}}}`)
	time.Sleep(50 * time.Millisecond)
	s.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":41}}`)
	text, isErr := toolText(t, s.recv())
	if !isErr || !strings.Contains(text, "context canceled") {
		t.Errorf("cancelled call: %q (error=%v)", text, isErr)
	}
}

func TestServeEndsWhenTheClientHangsUp(t *testing.T) {
	s := newSession(t, &fakeDaemon{})
	s.rpc(1, "ping", nil)
	s.in.Close()
	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("Serve returned %v at end of input", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return at end of input")
	}
}

func TestEveryByteOnTheOutputIsAProtocolMessage(t *testing.T) {
	d := &fakeDaemon{errs: map[string]error{"ticket.list": errors.New("x")}}
	s := newSession(t, d)
	s.rpc(1, "initialize", map[string]any{})
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.rpc(2, "tools/list", nil)
	s.callTool(3, "rig_list_tickets", map[string]any{})
	s.send("garbage")
	s.recv()
	// recv() itself fails the test on any line that is not a JSON-RPC 2.0 message
}
