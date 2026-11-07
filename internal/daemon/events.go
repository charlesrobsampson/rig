package daemon

import (
	"context"
	"encoding/json"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/events"
)

// EventsHandler streams the bus to one client: the subscribed ack, then every event
// until the client leaves. A subscriber the bus dropped for being too slow gets an
// unavailable error so its client knows to reconnect and refresh.
func EventsHandler(bus *events.Bus) StreamHandler {
	return func(ctx context.Context, _ json.RawMessage, send func(any) error) error {
		sub := bus.Subscribe()
		defer sub.Close()
		for {
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					if sub.Dropped() {
						return &api.Error{Code: api.CodeUnavailable, Message: "event subscriber fell behind and was dropped; reconnect and refresh"}
					}
					return nil
				}
				if err := send(ev); err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func RegisterEventHandlers(s *Server, bus *events.Bus) {
	s.HandleStream(api.MethodEventsSubscribe, EventsHandler(bus))
}
