package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func eventsHarness(t *testing.T) (*harness, *events.Bus) {
	t.Helper()
	bus := events.NewBus(testkit.NewFakeClock(epoch), 0)
	h := start(t, func(s *daemon.Server) { daemon.RegisterEventHandlers(s, bus) })
	return h, bus
}

func subscribe(t *testing.T, h *harness) (*client, string) {
	t.Helper()
	c := dial(t, h.opts.SocketPath)
	c.hello(t)
	id := c.send(t, api.MethodEventsSubscribe, nil)
	ev := readEvent(t, c, id)
	if ev.Type != api.EventSubscribed {
		t.Fatalf("first stream item = %s, want the subscribed ack", ev.Type)
	}
	return c, id
}

func readEvent(t *testing.T, c *client, id string) api.Event {
	t.Helper()
	r, err := c.dec.ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id || !r.IsStream() {
		t.Fatalf("unexpected message %+v", r)
	}
	var ev api.Event
	if err := json.Unmarshal(r.Stream, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestSubscribersReceiveEventsInOrder(t *testing.T) {
	h, bus := eventsHarness(t)
	a, aID := subscribe(t, h)
	b, bID := subscribe(t, h)

	for _, ticket := range []string{"t1", "t2", "t3"} {
		if err := bus.Publish(api.EventTicketCreated, ticket, "", api.EmptyData{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		cl   *client
		id   string
	}{{"first", a, aID}, {"second", b, bID}} {
		for _, want := range []string{"t1", "t2", "t3"} {
			if got := readEvent(t, c.cl, c.id).Ticket; got != want {
				t.Errorf("%s subscriber got %s, want %s", c.name, got, want)
			}
		}
	}
}

func TestDisconnectingUnsubscribes(t *testing.T) {
	h, bus := eventsHarness(t)
	c, _ := subscribe(t, h)
	if bus.Subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", bus.Subscribers())
	}
	c.conn.Close()
	deadline := time.After(5 * time.Second)
	for bus.Subscribers() != 0 {
		select {
		case <-deadline:
			t.Fatal("the subscription outlived its client")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestASubscriberThatCannotKeepUpIsToldToReconnect(t *testing.T) {
	bus := events.NewBus(testkit.NewFakeClock(epoch), 4)
	release := make(chan struct{})
	started := make(chan struct{})
	send := func(any) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // a client that is not reading
		return nil
	}

	result := make(chan error, 1)
	go func() { result <- daemon.EventsHandler(bus)(context.Background(), nil, send) }()
	<-started // the handler is stuck sending the ack

	for i := 0; i < 20; i++ {
		bus.Publish(api.EventTicketCreated, "t", "", api.EmptyData{})
	}
	close(release)

	select {
	case err := <-result:
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeUnavailable {
			t.Fatalf("handler returned %v, want an unavailable error telling the client to reconnect", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never noticed it was dropped")
	}
}

func TestShutdownTellsOpenStreamsTheDaemonIsStopping(t *testing.T) {
	h, _ := eventsHarness(t)
	c, id := subscribe(t, h)

	h.srv.Stop()
	r, err := c.dec.ReadResponse()
	if err != nil {
		t.Fatalf("expected a final message before the connection closed: %v", err)
	}
	var apiErr *api.Error
	if r.ID != id || !errors.As(r.Err(), &apiErr) || apiErr.Code != api.CodeUnavailable || !strings.Contains(apiErr.Message, "shutting down") {
		t.Errorf("final message = %+v, want an unavailable error saying the daemon is shutting down", r)
	}
}
