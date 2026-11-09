// Package api defines the socket protocol between the rig daemon and its clients:
// the request and response envelope, error codes, method names, the handshake, and
// events. It is pure types and a JSON-lines codec; docs/formats.md §8–9 is the spec.
package api

import (
	"encoding/json"
	"fmt"
)

const ProtocolVersion = 1

const (
	MethodHello             = "hello"
	MethodStatus            = "status"
	MethodConfigGet         = "config.get"
	MethodTicketList        = "ticket.list"
	MethodTicketGet         = "ticket.get"
	MethodTicketRescan      = "ticket.rescan"
	MethodTicketNew         = "ticket.new"
	MethodTicketStart       = "ticket.start"
	MethodTicketStop        = "ticket.stop"
	MethodGateList          = "gate.list"
	MethodGateApprove       = "gate.approve"
	MethodPhaseSet          = "phase.set"
	MethodTransitionRequest = "transition.request"
	MethodAgentAttach       = "agent.attach"
	MethodAgentMessage      = "agent.message"
	MethodAgentEvent        = "agent.event"
	MethodContractList      = "contract.list"
	MethodContractAccept    = "contract.accept"
	MethodContractReject    = "contract.reject"
	MethodContractAck       = "contract.ack"
	MethodReviewDiff        = "review.diff"
	MethodPushRun           = "push.run"
	MethodTestsRender       = "tests.render"
	MethodLogsStream        = "logs.stream"
	MethodEventsSubscribe   = "events.subscribe"
	MethodReconcileRun      = "reconcile.run"
	MethodDaemonStop        = "daemon.stop"
)

var methods = map[string]bool{
	MethodHello: true, MethodStatus: true, MethodConfigGet: true, MethodTicketList: true,
	MethodTicketGet: true, MethodTicketRescan: true, MethodTicketNew: true, MethodTicketStart: true,
	MethodTicketStop: true, MethodGateList: true, MethodGateApprove: true, MethodPhaseSet: true,
	MethodTransitionRequest: true, MethodAgentAttach: true, MethodAgentMessage: true,
	MethodAgentEvent: true, MethodContractList: true, MethodContractAccept: true,
	MethodContractReject: true, MethodContractAck: true, MethodReviewDiff: true, MethodPushRun: true, MethodTestsRender: true,
	MethodLogsStream: true, MethodEventsSubscribe: true, MethodReconcileRun: true, MethodDaemonStop: true,
}

func KnownMethod(m string) bool { return methods[m] }

// IsStreaming reports whether a method answers with many messages before an end marker.
func IsStreaming(m string) bool { return m == MethodEventsSubscribe || m == MethodLogsStream }

type ErrorCode string

const (
	CodeInvalid     ErrorCode = "invalid"
	CodeNotFound    ErrorCode = "not_found"
	CodeConflict    ErrorCode = "conflict"
	CodeBlocked     ErrorCode = "blocked"
	CodeUnavailable ErrorCode = "unavailable"
	CodeNotRunning  ErrorCode = "not_running"
	CodeInternal    ErrorCode = "internal"
)

// Error is the structured failure a response carries. For CodeBlocked, Details lists
// the reasons (a missing gate, a dirty checkout, a limit).
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Details []string  `json:"details,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

type Request struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Validate checks the envelope; it does not look at params.
func (r Request) Validate() *Error {
	switch {
	case r.V == 0:
		return &Error{Code: CodeInvalid, Message: `missing protocol version ("v")`}
	case r.V != ProtocolVersion:
		return &Error{Code: CodeInvalid, Message: fmt.Sprintf("unsupported protocol version %d (this daemon speaks %d)", r.V, ProtocolVersion)}
	case r.ID == "":
		return &Error{Code: CodeInvalid, Message: `"id" is required`}
	case r.Method == "":
		return &Error{Code: CodeInvalid, Message: `"method" is required`}
	case !KnownMethod(r.Method):
		return &Error{Code: CodeInvalid, Message: fmt.Sprintf("unknown method %q", r.Method)}
	}
	return nil
}

// Response is one line from the daemon. A plain answer has OK set; a stream sends
// messages with only Stream set and finishes with End.
type Response struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	OK     *bool           `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Stream json.RawMessage `json:"stream,omitempty"`
	End    bool            `json:"end,omitempty"`
}

func Success(id string, result any) (Response, error) {
	raw := json.RawMessage(`{}`)
	if result != nil {
		var err error
		if raw, err = json.Marshal(result); err != nil {
			return Response{}, fmt.Errorf("encode result: %w", err)
		}
	}
	ok := true
	return Response{V: ProtocolVersion, ID: id, OK: &ok, Result: raw}, nil
}

func Failure(id string, err *Error) Response {
	ok := false
	return Response{V: ProtocolVersion, ID: id, OK: &ok, Error: err}
}

func StreamItem(id string, item any) (Response, error) {
	raw, err := json.Marshal(item)
	if err != nil {
		return Response{}, fmt.Errorf("encode stream item: %w", err)
	}
	return Response{V: ProtocolVersion, ID: id, Stream: raw}, nil
}

func EndOfStream(id string) Response {
	ok := true
	return Response{V: ProtocolVersion, ID: id, OK: &ok, End: true}
}

func (r Response) IsStream() bool { return len(r.Stream) > 0 }
func (r Response) IsEnd() bool    { return r.End }

// Err is the structured error of a failed response, or nil.
func (r Response) Err() error {
	if r.Error == nil {
		return nil
	}
	return r.Error
}

type HelloParams struct {
	Client  string `json:"client"`
	Version string `json:"version"`
}

type HelloResult struct {
	Protocol int    `json:"protocol"`
	Server   string `json:"server"`
}

// Compatible reports whether a client can talk to the server that sent this result.
func (h HelloResult) Compatible() error {
	if h.Protocol != ProtocolVersion {
		return fmt.Errorf("server speaks protocol version %d, this client speaks %d", h.Protocol, ProtocolVersion)
	}
	return nil
}
