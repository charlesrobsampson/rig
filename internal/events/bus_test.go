package events_test

import (
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func newBus(buffer int) (*events.Bus, *testkit.FakeClock) {
	c := testkit.NewFakeClock(epoch)
	return events.NewBus(c, buffer), c
}

// next returns the next event or fails if none arrives.
func next(t *testing.T, sub *events.Subscription) api.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
	}
	return api.Event{}
}

func publish(t *testing.T, b *events.Bus, typ api.EventType, ticket string) {
	t.Helper()
	if err := b.Publish(typ, ticket, "", api.EmptyData{}); err != nil {
		t.Fatal(err)
	}
}

func TestSubscribersGetAnAckThenEventsInOrder(t *testing.T) {
	b, c := newBus(0)
	sub := b.Subscribe()
	defer sub.Close()

	if ev := next(t, sub); ev.Type != api.EventSubscribed {
		t.Fatalf("first event = %s, want the subscribed ack", ev.Type)
	}
	c.Advance(time.Minute)
	for _, ticket := range []string{"t1", "t2", "t3"} {
		publish(t, b, api.EventTicketCreated, ticket)
	}
	for _, want := range []string{"t1", "t2", "t3"} {
		ev := next(t, sub)
		if ev.Ticket != want || ev.Type != api.EventTicketCreated {
			t.Errorf("event = %s/%s, want ticket.created for %s", ev.Type, ev.Ticket, want)
		}
		if !ev.At.Equal(epoch.Add(time.Minute)) {
			t.Errorf("event time = %v, want the clock's %v", ev.At, epoch.Add(time.Minute))
		}
	}
}

func TestEventDataIsCarriedAndDecodable(t *testing.T) {
	b, _ := newBus(0)
	sub := b.Subscribe()
	defer sub.Close()
	next(t, sub) // ack
	if err := b.Publish(api.EventPhaseChanged, "t1", "", api.PhaseChangedData{From: domain.PhasePlan, To: domain.PhaseImplement, By: "human"}); err != nil {
		t.Fatal(err)
	}
	var data api.PhaseChangedData
	if err := next(t, sub).DecodeData(&data); err != nil || data.To != domain.PhaseImplement {
		t.Errorf("data = %+v, %v", data, err)
	}
}

func TestEverySubscriberSeesEveryEventPublishedAfterItJoined(t *testing.T) {
	b, _ := newBus(0)
	publish(t, b, api.EventTicketCreated, "before-anyone")

	first := b.Subscribe()
	defer first.Close()
	next(t, first)
	publish(t, b, api.EventTicketCreated, "one")

	second := b.Subscribe()
	defer second.Close()
	next(t, second)
	publish(t, b, api.EventTicketCreated, "two")

	if got := []string{next(t, first).Ticket, next(t, first).Ticket}; got[0] != "one" || got[1] != "two" {
		t.Errorf("first saw %v, want [one two]", got)
	}
	if got := next(t, second).Ticket; got != "two" {
		t.Errorf("second saw %q first, want two (nothing from before it joined)", got)
	}
}

func TestASlowSubscriberIsDroppedWithoutBlockingPublishersOrOthers(t *testing.T) {
	b, _ := newBus(4)
	slow := b.Subscribe() // never read
	fast := b.Subscribe()
	defer fast.Close()
	next(t, fast) // ack

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			publish(t, b, api.EventTicketCreated, "t")
			next(t, fast) // the healthy subscriber keeps up
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a slow subscriber blocked the publisher")
	}

	if !slow.Dropped() {
		t.Error("the slow subscriber was not marked dropped")
	}
	n := 0
	for range slow.Events() { // drains what was buffered, then the channel closes
		n++
	}
	if n == 0 || n > 4 {
		t.Errorf("slow subscriber drained %d events, want between 1 and its buffer of 4", n)
	}
	if b.Subscribers() != 1 {
		t.Errorf("subscribers = %d, want only the healthy one", b.Subscribers())
	}
	publish(t, b, api.EventTicketCreated, "after-drop") // must not panic or block
}

func TestCloseUnsubscribes(t *testing.T) {
	b, _ := newBus(0)
	sub := b.Subscribe()
	if b.Subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", b.Subscribers())
	}
	sub.Close()
	sub.Close() // idempotent
	if b.Subscribers() != 0 {
		t.Errorf("subscribers = %d after Close, want 0", b.Subscribers())
	}
	if sub.Dropped() {
		t.Error("a subscriber that closed itself was not dropped")
	}
	for range sub.Events() { // the channel must be closed so readers stop
	}
	publish(t, b, api.EventTicketCreated, "t") // nobody listening: fine
}

func TestPublishReportsDataThatCannotBeEncoded(t *testing.T) {
	b, _ := newBus(0)
	if err := b.Publish(api.EventError, "t", "", make(chan int)); err == nil {
		t.Error("expected an encoding error")
	}
}

func TestConcurrentPublishersAndSubscribers(t *testing.T) {
	b, _ := newBus(10000)
	stable := b.Subscribe()
	defer stable.Close()
	next(t, stable)

	const publishers, each = 8, 100
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				publish(t, b, api.EventTicketCreated, "t")
			}
		}()
		go func() { // subscribers coming and going while events flow
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s := b.Subscribe()
				s.Close()
			}
		}()
	}
	wg.Wait()

	for i := 0; i < publishers*each; i++ {
		next(t, stable)
	}
}
