package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

// RegisterTimelineHandler serves timeline.list: a ticket's timeline after a seq.
func RegisterTimelineHandler(s *Server, ticketsDir string) {
	s.Handle(api.MethodTimelineList, func(_ context.Context, params json.RawMessage) (any, error) {
		var p api.TimelineListParams
		if err := json.Unmarshal(params, &p); err != nil || p.Ticket == "" {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "timeline.list needs params like {\"ticket\": \"t-1\", \"since\": 12}"}
		}
		if p.Since < 0 {
			return nil, &api.Error{Code: api.CodeInvalid, Message: "since must be 0 (everything) or a seq"}
		}
		if err := domain.ValidateTicketID(p.Ticket); err != nil {
			return nil, &api.Error{Code: api.CodeInvalid, Message: err.Error()}
		}
		dir := filepath.Join(ticketsDir, p.Ticket)
		if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
			return nil, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q not found", p.Ticket)}
		}
		entries, err := timeline.Read(dir, p.Since)
		if err != nil {
			return nil, err
		}
		res := api.TimelineListResult{Entries: []api.TimelineEntry{}}
		for _, e := range entries {
			res.Entries = append(res.Entries, api.TimelineEntry(e))
		}
		return res, nil
	})
}
