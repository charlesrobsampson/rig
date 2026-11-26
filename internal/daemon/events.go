package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

// EventsOptions are what events.subscribe needs beyond the bus.
type EventsOptions struct {
	// TicketsDir is where the tickets live; without it a replay (params.since) is refused.
	TicketsDir string
	// AfterSubscribe, when set, runs once the subscription is live and before the replay
	// is read: tests use it to append at exactly the switch-over.
	AfterSubscribe func()
}

// EventsHandler streams the bus to one client: the subscribed ack, then every event
// until the client leaves. A subscriber the bus dropped for being too slow gets an
// unavailable error so its client knows to reconnect and refresh.
func EventsHandler(bus *events.Bus) StreamHandler {
	return NewEventsHandler(bus, EventsOptions{})
}

// NewEventsHandler is EventsHandler with the optional params of docs/formats.md §8:
// a ticket filter and a replay of each ticket's timeline after a seq, followed by the
// live stream with no gap and no repeat. The order that makes it so: subscribe to the
// bus first, read the files second, then drop live entries the replay already sent.
// An entry appended before the subscription is in the file; one appended after it is in
// the channel; one in between is in both and is sent once.
func NewEventsHandler(bus *events.Bus, opts EventsOptions) StreamHandler {
	return func(ctx context.Context, params json.RawMessage, send func(any) error) error {
		var p api.EventsSubscribeParams
		if len(params) > 0 && string(params) != "null" {
			if err := json.Unmarshal(params, &p); err != nil {
				return &api.Error{Code: api.CodeInvalid, Message: "events.subscribe takes params like {\"tickets\": [\"t-1\"], \"since\": {\"t-1\": 12}}"}
			}
		}
		watch := map[string]bool{}
		for _, id := range p.Tickets {
			watch[id] = true
		}
		for id, seq := range p.Since {
			if seq < 0 {
				return &api.Error{Code: api.CodeInvalid, Message: "since must be 0 (everything) or a seq"}
			}
			watch[id] = true
		}
		if len(p.Since) > 0 && opts.TicketsDir == "" {
			return &api.Error{Code: api.CodeInvalid, Message: "this daemon cannot replay timelines"}
		}
		for id := range watch {
			if err := domain.ValidateTicketID(id); err != nil {
				return &api.Error{Code: api.CodeInvalid, Message: err.Error()}
			}
			if opts.TicketsDir != "" {
				if _, err := os.Stat(filepath.Join(opts.TicketsDir, id, "state.json")); err != nil {
					return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q not found", id)}
				}
			}
		}

		sub := bus.Subscribe()
		defer sub.Close()
		if opts.AfterSubscribe != nil {
			opts.AfterSubscribe()
		}

		// sent is the highest timeline seq delivered per ticket.
		sent := map[string]int{}
		sendEntry := func(ticket string, e timeline.Entry) error {
			ev, err := api.NewEvent(api.EventTimelineEntry, ticket, e.Service, e.At, api.TimelineEntryData{Ticket: ticket, Seq: e.Seq, Entry: api.TimelineEntry(e)})
			if err != nil {
				return err
			}
			sent[ticket] = e.Seq
			return send(ev)
		}
		replay := func(ticket string, after, upTo int) error {
			entries, err := timeline.Read(filepath.Join(opts.TicketsDir, ticket), after)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if upTo > 0 && e.Seq > upTo {
					break
				}
				if err := sendEntry(ticket, e); err != nil {
					return err
				}
			}
			return nil
		}

		// The ack is the first thing in the channel; send it before the replay so a
		// client knows the subscription is live.
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return dropped(sub)
			}
			if err := send(ev); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		ids := make([]string, 0, len(p.Since))
		for id := range p.Since {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			sent[id] = p.Since[id]
			if err := replay(id, p.Since[id], 0); err != nil {
				return err
			}
		}

		for {
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					return dropped(sub)
				}
				if len(watch) > 0 && ev.Ticket != "" && !watch[ev.Ticket] {
					continue
				}
				if ev.Type == api.EventTimelineEntry {
					var d api.TimelineEntryData
					if err := ev.DecodeData(&d); err != nil {
						return err
					}
					last, known := sent[ev.Ticket]
					if known && d.Seq <= last {
						continue // the replay already sent it
					}
					if known && d.Seq > last+1 && opts.TicketsDir != "" {
						// A hole should not happen; fill it from the file rather than leave it.
						if err := replay(ev.Ticket, last, d.Seq-1); err != nil {
							return err
						}
					}
					sent[ev.Ticket] = d.Seq
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

func dropped(sub *events.Subscription) error {
	if sub.Dropped() {
		return &api.Error{Code: api.CodeUnavailable, Message: "event subscriber fell behind and was dropped; reconnect and refresh"}
	}
	return nil
}

func RegisterEventHandlers(s *Server, bus *events.Bus) {
	RegisterEventHandlersWith(s, bus, EventsOptions{})
}

// RegisterEventHandlersWith serves events.subscribe with replay from opts.TicketsDir.
func RegisterEventHandlersWith(s *Server, bus *events.Bus, opts EventsOptions) {
	s.HandleStream(api.MethodEventsSubscribe, NewEventsHandler(bus, opts))
}
