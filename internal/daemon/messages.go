package daemon

import (
	"context"
	"encoding/json"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/inject"
)

// RegisterMessageHandler serves agent.message: type a message into a running agent.
func RegisterMessageHandler(s *Server, in *inject.Injector) {
	s.Handle(api.MethodAgentMessage, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p api.AgentMessageParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "agent.message needs params like {\"ticket\": \"t-1\", \"service\": \"api\", \"text\": \"...\"}"}
		}
		return in.Send(ctx, p, "human")
	})
}
