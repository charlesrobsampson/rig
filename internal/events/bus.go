// Package events is the daemon's in-process event bus. Components publish; every
// events.subscribe stream is a subscriber. Publishing never blocks: a subscriber that
// cannot keep up is dropped and must reconnect and refresh.
package events

import (
	"sync"
	"sync/atomic"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
)

const defaultBuffer = 256

type Bus struct {
	clock  clock.Clock
	buffer int

	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// NewBus makes a bus whose subscribers each buffer up to buffer events (0 means 256).
func NewBus(c clock.Clock, buffer int) *Bus {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	return &Bus{clock: c, buffer: buffer, subs: map[*Subscription]struct{}{}}
}

// Publish stamps the event with the bus clock and hands it to every subscriber. It
// fails only if data cannot be encoded as JSON.
func (b *Bus) Publish(typ api.EventType, ticket, service string, data any) error {
	ev, err := api.NewEvent(typ, ticket, service, b.clock.Now(), data)
	if err != nil {
		return err
	}
	// Holding the lock while sending keeps every subscriber's order identical to
	// publish order; the send itself never blocks.
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			s.dropped.Store(true)
			b.remove(s)
		}
	}
	return nil
}

// Subscribe starts receiving events published from now on. The first event delivered
// is always the subscribed ack, so a client that has seen it knows nothing later
// will be missed.
func (b *Bus) Subscribe() *Subscription {
	s := &Subscription{bus: b, ch: make(chan api.Event, b.buffer)}
	ack, _ := api.NewEvent(api.EventSubscribed, "", "", b.clock.Now(), api.EmptyData{})
	s.ch <- ack
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// remove needs b.mu held and closes the channel exactly once, since only a
// subscription still in the map is removed.
func (b *Bus) remove(s *Subscription) {
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.ch)
	}
}

type Subscription struct {
	bus     *Bus
	ch      chan api.Event
	dropped atomic.Bool
}

// Events is closed when the subscription ends, either by Close or by being dropped.
func (s *Subscription) Events() <-chan api.Event { return s.ch }

// Dropped reports that the bus ended this subscription because it fell behind.
func (s *Subscription) Dropped() bool { return s.dropped.Load() }

func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.bus.remove(s)
}
