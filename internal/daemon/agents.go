package daemon

import (
	"context"
	"encoding/json"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/supervisor"
)

// RegisterAgentHandlers serves agent.event: the report an agent's hook makes about
// its own state.
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
}
