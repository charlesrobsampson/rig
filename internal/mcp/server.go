// Package mcp serves rig's daemon API to a supervising agent over the Model Context
// Protocol (stdio transport, JSON-RPC 2.0, one message per line). It is a thin wrapper:
// every tool is one daemon call, so an orchestrating agent can do exactly what a CLI
// user can, except decide. There are deliberately no tools that approve a gate, move a
// ticket without the gate rules, accept or reject a contract change, sign off tests,
// push or stop: those stay with the human.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Daemon is the part of the daemon client the tools use.
type Daemon interface {
	Call(ctx context.Context, method string, params, result any) error
}

type Options struct {
	Daemon  Daemon
	Version string // rig's version, reported to the client
	// Stderr receives diagnostics (never stdout: that is the protocol).
	Stderr io.Writer
}

// Server speaks MCP on a pair of streams.
type Server struct {
	o     Options
	tools []Tool
	mu    sync.Mutex // serializes writes to the output
	out   *json.Encoder

	cancelMu sync.Mutex
	inflight map[string]context.CancelFunc
}

func NewServer(o Options) *Server {
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	return &Server{o: o, tools: tools(o.Daemon), inflight: map[string]context.CancelFunc{}}
}

// Tools lists the tools the server offers, in order.
func (s *Server) Tools() []Tool { return s.tools }

// latest is the newest protocol revision this server speaks; older ones it also accepts.
const latest = "2025-06-18"

var supported = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from in and answers on out until in ends or ctx is cancelled.
// Each request runs on its own goroutine, so a slow tool never blocks ping or the
// others; the function returns after every request has been answered.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reader := bufio.NewReaderSize(in, 1<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		line, err := readLine(reader)
		if len(line) > 0 {
			wg.Add(1)
			go func(line []byte) {
				defer wg.Done()
				s.handle(ctx, line)
			}(line)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// maxLine bounds one message; a client that sends more is broken or hostile.
const maxLine = 4 << 20

// readLine returns the next non-empty line without its newline. A line over maxLine is
// skipped (the next call carries on after it), reported as an error response by handle.
func readLine(r *bufio.Reader) ([]byte, error) {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > maxLine {
			line = []byte(`{"jsonrpc":"2.0","method":"\u0000too-large"}`)
		}
		trimmed := trimSpace(line)
		if len(trimmed) > 0 || err != nil {
			return trimmed, err
		}
	}
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

func (s *Server) handle(ctx context.Context, line []byte) {
	if len(line) > 0 && line[0] == '[' {
		s.reply(nil, nil, &rpcError{codeInvalidRequest, "batch requests are not supported"})
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(nil, nil, &rpcError{codeParse, "invalid JSON: " + err.Error()})
		return
	}
	if req.Method == "\x00too-large" {
		s.reply(nil, nil, &rpcError{codeInvalidRequest, "message too large"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) > 0 {
			s.reply(req.ID, nil, &rpcError{codeInvalidRequest, "not a JSON-RPC 2.0 request"})
		}
		return
	}
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "notifications/cancelled":
		s.cancel(req.Params)
		return
	case "initialize":
		s.initialize(req)
		return
	case "ping":
		if !isNotification {
			s.reply(req.ID, struct{}{}, nil)
		}
		return
	case "tools/list":
		if !isNotification {
			s.reply(req.ID, map[string]any{"tools": s.tools}, nil)
		}
		return
	case "tools/call":
		if isNotification {
			return
		}
		s.call(ctx, req)
		return
	}
	if isNotification {
		return // notifications/initialized and anything else we do not act on
	}
	s.reply(req.ID, nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method})
}

func (s *Server) initialize(req request) {
	if len(req.ID) == 0 {
		return
	}
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(req.Params, &p)
	version := latest
	if supported[p.ProtocolVersion] {
		version = p.ProtocolVersion // the client's own revision, when we speak it
	}
	s.reply(req.ID, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "rig", "version": s.o.Version},
		"instructions": "Supervise rig's coding agents: list tickets, read a ticket's status and files, message an agent, " +
			"request a phase change, or propose a contract change. Approving gates, accepting contract changes, " +
			"signing off tests, pushing and stopping are done by the human, never through this server.",
	}, nil)
}

func (s *Server) call(ctx context.Context, req request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.reply(req.ID, nil, &rpcError{codeInvalidParams, "tools/call needs a name and arguments"})
		return
	}
	var tool *Tool
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			tool = &s.tools[i]
		}
	}
	if tool == nil {
		s.reply(req.ID, nil, &rpcError{codeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name)})
		return
	}

	cctx, cancel := context.WithCancel(ctx)
	key := string(req.ID)
	s.cancelMu.Lock()
	s.inflight[key] = cancel
	s.cancelMu.Unlock()
	defer func() {
		s.cancelMu.Lock()
		delete(s.inflight, key)
		s.cancelMu.Unlock()
		cancel()
	}()

	text, err := safeRun(cctx, tool, p.Arguments)
	if err != nil {
		// a failed tool is a result the model can read and react to, not a protocol error
		s.reply(req.ID, map[string]any{"isError": true, "content": []any{textContent(errorText(err))}}, nil)
		return
	}
	s.reply(req.ID, map[string]any{"content": []any{textContent(text)}}, nil)
}

// safeRun runs a tool, turning a panic into an error so one bad call never ends the server.
func safeRun(ctx context.Context, t *Tool, args json.RawMessage) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error in %s: %v", t.Name, r)
		}
	}()
	return t.run(ctx, args)
}

func textContent(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func (s *Server) cancel(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.cancelMu.Lock()
	cancel := s.inflight[string(p.RequestID)]
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) reply(id json.RawMessage, result any, e *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := response{JSONRPC: "2.0", ID: id, Result: result, Error: e}
	if e == nil && result == nil {
		resp.Result = struct{}{}
	}
	if err := s.out.Encode(resp); err != nil {
		fmt.Fprintf(s.o.Stderr, "rig mcp: cannot write a response: %v\n", err)
	}
}
