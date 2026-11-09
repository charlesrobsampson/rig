package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// Tool is one thing the supervising agent can do.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`

	run func(ctx context.Context, args json.RawMessage) (string, error)
}

// ToolNames is the documented set, in order (docs/mcp.md). There is no tool to approve a
// gate, set a phase past the gate rules, accept or reject a contract change, sign off the
// manual tests, push, or stop a ticket.
var ToolNames = []string{
	"rig_list_tickets",
	"rig_get_ticket",
	"rig_read_artifact",
	"rig_send_message",
	"rig_request_transition",
	"rig_propose_contract_change",
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func readOnly() map[string]any {
	return map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
}

func writes() map[string]any {
	return map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false}
}

func tools(d Daemon) []Tool {
	call := func(ctx context.Context, method string, params, result any) error {
		if d == nil {
			return errors.New("no daemon connection configured")
		}
		return d.Call(ctx, method, params, result)
	}
	return []Tool{
		{
			Name:        "rig_list_tickets",
			Description: "List every rig ticket with its phase, services (agent state, host port), whether a contract change is pending, which approval a human still owes, and any summary.",
			InputSchema: obj(map[string]any{}),
			Annotations: readOnly(),
			run: func(ctx context.Context, args json.RawMessage) (string, error) {
				if err := noArgs(args); err != nil {
					return "", err
				}
				var res api.TicketListResult
				if err := call(ctx, api.MethodTicketList, nil, &res); err != nil {
					return "", err
				}
				return pretty(res)
			},
		},
		{
			Name:        "rig_get_ticket",
			Description: "Get one ticket's status: its summary row, its recorded state (phase, per-service agent state, approvals, contract and manual-test progress) and the gates it needs.",
			InputSchema: obj(map[string]any{"ticket": str("the ticket id, like ticket-412")}, "ticket"),
			Annotations: readOnly(),
			run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Ticket string `json:"ticket"`
				}
				if err := decode(raw, &a, "ticket"); err != nil {
					return "", err
				}
				var got api.TicketGetResult
				if err := call(ctx, api.MethodTicketGet, api.TicketGetParams{ID: a.Ticket}, &got); err != nil {
					return "", err
				}
				out := map[string]any{"summary": got.Summary, "state": got.State}
				var gates api.GateListResult
				if err := call(ctx, api.MethodGateList, api.GateListParams{ID: a.Ticket}, &gates); err == nil {
					out["gates"] = gates
				}
				return pretty(out)
			},
		},
		{
			Name: "rig_read_artifact",
			Description: "Read one of a ticket's lifecycle files: plan.md, tasks.md, contract.md (multi-service tickets only) or manual-tests.md. " +
				"These are the only files it can read.",
			InputSchema: obj(map[string]any{
				"ticket": str("the ticket id"),
				"name":   map[string]any{"type": "string", "enum": api.TicketFileNames, "description": "which file"},
			}, "ticket", "name"),
			Annotations: readOnly(),
			run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Ticket, Name string }
				if err := decode(raw, &a, "ticket", "name"); err != nil {
					return "", err
				}
				var res api.TicketFileResult
				if err := call(ctx, api.MethodTicketFile, api.TicketFileParams{Ticket: a.Ticket, Name: a.Name}, &res); err != nil {
					return "", err
				}
				return res.Content, nil
			},
		},
		{
			Name: "rig_send_message",
			Description: "Type a message into a service's running coding agent, as if the human had typed it. Refused while the agent waits on a permission prompt " +
				"(the keystroke would approve it); it waits briefly for an agent that is still starting. Keep messages short and concrete.",
			InputSchema: obj(map[string]any{
				"ticket":  str("the ticket id"),
				"service": str("the service whose agent to message"),
				"text":    str("the message"),
			}, "ticket", "service", "text"),
			Annotations: writes(),
			run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Ticket, Service, Text string }
				if err := decode(raw, &a, "ticket", "service", "text"); err != nil {
					return "", err
				}
				var res api.AgentMessageResult
				if err := call(ctx, api.MethodAgentMessage, api.AgentMessageParams{Ticket: a.Ticket, Service: a.Service, Text: a.Text}, &res); err != nil {
					return "", err
				}
				return fmt.Sprintf("sent to %s/%s", a.Ticket, res.Service), nil
			},
		},
		{
			Name: "rig_request_transition",
			Description: "Ask for a ticket to move to another phase on behalf of a service, under exactly the rules a human's move follows: a forward move whose gate " +
				"has not been approved by a human is refused (blocked) and listed; a backward move needs a reason. This never approves anything.",
			InputSchema: obj(map[string]any{
				"ticket":  str("the ticket id"),
				"service": str("the service on whose behalf the move is requested"),
				"to":      map[string]any{"type": "string", "enum": []string{"plan", "implement", "test", "review", "push", "done"}, "description": "the phase to move to"},
				"reason":  str("why (required when moving backward)"),
			}, "ticket", "service", "to"),
			Annotations: writes(),
			run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Ticket, Service, To, Reason string }
				if err := decode(raw, &a, "ticket", "service", "to"); err != nil {
					return "", err
				}
				to := domain.Phase(a.To)
				if !to.Valid() {
					return "", fmt.Errorf("%q is not a phase (plan, implement, test, review, push, done)", a.To)
				}
				var res api.TransitionResult
				if err := call(ctx, api.MethodTransitionRequest, api.TransitionRequestParams{ID: a.Ticket, Service: a.Service, To: to, Reason: a.Reason}, &res); err != nil {
					return "", err
				}
				return pretty(res)
			},
		},
		{
			Name: "rig_propose_contract_change",
			Description: "Propose a change to a multi-service ticket's contract on behalf of a service. It is appended as an open proposal; it never edits the agreed section. " +
				"A human accepts or rejects it, and the other services' agents are told.",
			InputSchema: obj(map[string]any{
				"ticket":  str("the ticket id"),
				"service": str("the proposing service"),
				"what":    str("the change, precisely (endpoint, field, schema)"),
				"why":     str("why the change is needed"),
			}, "ticket", "service", "what", "why"),
			Annotations: writes(),
			run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Ticket, Service, What, Why string }
				if err := decode(raw, &a, "ticket", "service", "what", "why"); err != nil {
					return "", err
				}
				var res api.ContractProposeResult
				if err := call(ctx, api.MethodContractPropose, api.ContractProposeParams{Ticket: a.Ticket, Service: a.Service, What: a.What, Why: a.Why}, &res); err != nil {
					return "", err
				}
				return fmt.Sprintf("proposed %s; a human must accept it before it is part of the contract", res.Proposal), nil
			},
		},
	}
}

// decode reads a tool's arguments strictly: unknown names are an error, and every
// required string must be present and not blank.
func decode(raw json.RawMessage, into any, required ...string) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	var generic map[string]json.RawMessage
	json.Unmarshal(raw, &generic)
	for _, name := range required {
		v, ok := generic[name]
		var s string
		if !ok || json.Unmarshal(v, &s) != nil || strings.TrimSpace(s) == "" {
			return fmt.Errorf("bad arguments: %q is required (a non-empty string)", name)
		}
	}
	return nil
}

func noArgs(raw json.RawMessage) error {
	var m map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	if len(m) > 0 {
		return errors.New("bad arguments: this tool takes none")
	}
	return nil
}

func pretty(v any) (string, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	return string(raw), err
}

// errorText is how a failure reads to the model: the daemon's error with its code and any
// reasons, or the plain message of anything else.
func errorText(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		text := fmt.Sprintf("rig error (%s): %s", apiErr.Code, apiErr.Message)
		for _, d := range apiErr.Details {
			text += "\n- " + d
		}
		return text
	}
	return "error: " + strings.ReplaceAll(err.Error(), "\n", " ")
}
