package daemon

import (
	"context"
	"encoding/json"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/supervisor"
)

// RegisterAgentHandlers serves agent.event, the report an agent's hook makes about its
// own state; agent.record, a record a host agent adds to its ticket's journal; and
// journal.list.
func RegisterAgentHandlers(s *Server, sup *supervisor.Supervisor) {
	s.Handle(api.MethodAgentEvent, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.AgentEventParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "agent.event needs params like {\"ticket\": \"t-1\", \"service\": \"api\", \"state\": \"idle\"}"}
		}
		if err := sup.Report(ctx, p); err != nil {
			return nil, err
		}
		return api.EmptyData{}, nil
	})
	s.Handle(api.MethodAgentRecord, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.AgentRecordParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "agent.record needs params like {\"ticket\": \"t-1\", \"service\": \"api\", \"kind\": \"feedback\", \"text\": \"...\"}"}
		}
		if err := sup.Record(ctx, p); err != nil {
			return nil, err
		}
		return api.EmptyData{}, nil
	})
	s.Handle(api.MethodJournalList, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.JournalListParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &api.Error{Code: api.CodeInvalid, Message: "journal.list takes params like {\"ticket\": \"t-1\", \"kind\": \"feedback\", \"limit\": 20}"}
			}
		}
		return sup.Journal(p)
	})
}
