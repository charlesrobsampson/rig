package daemon

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
)

// RegisterTicketHandlers serves ticket.list, ticket.rescan and ticket.get from the
// registry. The registry reads the ticket files on every call, so rescan is the same
// fresh read as list; it exists so clients can ask for one explicitly.
func RegisterTicketHandlers(s *Server, r *registry.Registry) {
	list := func(context.Context, json.RawMessage) (any, error) {
		tickets, err := r.List()
		if err != nil {
			return nil, err
		}
		return api.TicketListResult{Tickets: tickets}, nil
	}
	s.Handle(api.MethodTicketList, list)
	s.Handle(api.MethodTicketRescan, list)

	s.Handle(api.MethodTicketGet, func(_ context.Context, params json.RawMessage) (any, error) {
		var p api.TicketGetParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "ticket.get needs params like {\"id\": \"ticket-1\"}"}
		}
		if err := domain.ValidateTicketID(p.ID); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: err.Error()}
		}
		res, err := r.Get(p.ID)
		if errors.Is(err, registry.ErrNotFound) {
			return nil, &api.Error{Code: api.CodeNotFound, Message: err.Error()}
		}
		return res, err
	})
}

// RegisterTicketCreateHandler serves ticket.new: create the ticket's files, then
// announce it on the bus. Nothing is announced for a failed create.
func RegisterTicketCreateHandler(s *Server, c *registry.Creator, bus *events.Bus) {
	s.Handle(api.MethodTicketNew, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.TicketNewParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "ticket.new needs params like {\"id\": \"...\", \"title\": \"...\", \"services\": [...]}"}
		}
		res, err := c.Create(ctx, p)
		if err != nil {
			return nil, err
		}
		if err := bus.Publish(api.EventTicketCreated, res.Summary.ID, "", api.EmptyData{}); err != nil {
			return nil, err
		}
		return res, nil
	})
}

// RegisterLifecycleHandlers serves ticket.start, ticket.stop, gate.list, gate.approve,
// phase.set and transition.request. gate.approve and phase.set are human actions and
// must never be exposed to agents; transition.request is the agents' way to ask.
func RegisterLifecycleHandlers(s *Server, m *lifecycle.Manager) {
	s.Handle(api.MethodTicketStart, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.TicketStartParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "ticket.start needs params like {\"id\": \"ticket-1\"}"}
		}
		return m.Start(ctx, p)
	})
	registerStop(s, m)
	s.HandleStream(api.MethodLogsStream, func(ctx context.Context, params json.RawMessage, send func(any) error) error {
		var p api.LogsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return &api.Error{Code: api.CodeInvalid, Message: "logs.stream needs params like {\"ticket\": \"ticket-1\", \"follow\": true}"}
		}
		return m.StreamLogs(ctx, p, send)
	})
	s.Handle(api.MethodAgentAttach, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.AgentAttachParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "agent.attach needs params like {\"ticket\": \"ticket-1\", \"service\": \"api\"}"}
		}
		return m.Attach(ctx, p)
	})
	s.Handle(api.MethodGateList, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.GateListParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "gate.list needs params like {\"id\": \"ticket-1\"}"}
		}
		return m.Gates(ctx, p)
	})
	s.Handle(api.MethodGateApprove, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.GateApproveParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "gate.approve needs params like {\"id\": \"ticket-1\", \"gate\": \"plan_approval\"}"}
		}
		return m.Approve(ctx, p)
	})
	s.Handle(api.MethodPhaseSet, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.PhaseSetParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "phase.set needs params like {\"id\": \"ticket-1\", \"to\": \"implement\"}"}
		}
		return m.SetPhase(ctx, p)
	})
	s.Handle(api.MethodTransitionRequest, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.TransitionRequestParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "transition.request needs params like {\"id\": \"ticket-1\", \"service\": \"api\", \"to\": \"test\"}"}
		}
		return m.RequestTransition(ctx, p)
	})
}

func registerStop(s *Server, m *lifecycle.Manager) {
	s.Handle(api.MethodTicketStop, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.TicketStopParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "ticket.stop needs params like {\"id\": \"ticket-1\"}"}
		}
		return m.Stop(ctx, p)
	})
}
