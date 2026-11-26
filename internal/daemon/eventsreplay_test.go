package daemon_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

type replayEnv struct {
	h   *harness
	dir string
	tl  *timeline.Log
	bus *events.Bus
}

func (e *replayEnv) ticket(t *testing.T, id string) string {
	t.Helper()
	d := filepath.Join(e.dir, id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *replayEnv) append(t *testing.T, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := e.tl.Append(filepath.Join(e.dir, id), timeline.Entry{Kind: timeline.TicketStarted}); err != nil {
			t.Fatal(err)
		}
	}
}

func newReplayEnv(t *testing.T, after func()) *replayEnv {
	t.Helper()
	e := &replayEnv{dir: t.TempDir()}
	clk := testkit.NewFakeClock(epoch)
	e.bus = events.NewBus(clk, 4096)
	e.tl = timeline.New(clk.Now)
	e.tl.OnAppend = func(d string, en timeline.Entry) {
		id := filepath.Base(d)
		e.bus.Publish(api.EventTimelineEntry, id, en.Service, api.TimelineEntryData{Ticket: id, Seq: en.Seq, Entry: api.TimelineEntry(en)})
	}
	e.h = start(t, func(s *daemon.Server) {
		daemon.RegisterEventHandlersWith(s, e.bus, daemon.EventsOptions{TicketsDir: e.dir, AfterSubscribe: func() {
			if after != nil {
				after()
			}
		}})
	})
	return e
}

func subscribeWith(t *testing.T, e *replayEnv, p api.EventsSubscribeParams) (*client, string) {
	t.Helper()
	c := dial(t, e.h.opts.SocketPath)
	c.hello(t)
	id := c.send(t, api.MethodEventsSubscribe, p)
	if ev := readEvent(t, c, id); ev.Type != api.EventSubscribed {
		t.Fatalf("first item %s", ev.Type)
	}
	return c, id
}

func readEntry(t *testing.T, c *client, id string) (string, int) {
	t.Helper()
	ev := readEvent(t, c, id)
	if ev.Type != api.EventTimelineEntry {
		t.Fatalf("got %s, want timeline.entry", ev.Type)
	}
	var d api.TimelineEntryData
	if err := ev.DecodeData(&d); err != nil {
		t.Fatal(err)
	}
	if d.Ticket != ev.Ticket || d.Entry.Seq != d.Seq {
		t.Fatalf("data %+v does not match envelope %+v", d, ev)
	}
	return d.Ticket, d.Seq
}

func TestReplayThenLiveHasNoGapAndNoRepeatWhenAnAppendRacesTheSwitchOver(t *testing.T) {
	var e *replayEnv
	// One entry lands after the subscription is live and before the replay reads the
	// file: it is in both, and must arrive once.
	e = newReplayEnv(t, func() { e.append(t, "t1", 1) })
	e.ticket(t, "t1")
	e.append(t, "t1", 3)
	c, id := subscribeWith(t, e, api.EventsSubscribeParams{Since: map[string]int{"t1": 1}})
	for want := 2; want <= 4; want++ {
		if tk, seq := readEntry(t, c, id); tk != "t1" || seq != want {
			t.Fatalf("got %s/%d, want t1/%d", tk, seq, want)
		}
	}
	e.append(t, "t1", 2)
	for want := 5; want <= 6; want++ {
		if _, seq := readEntry(t, c, id); seq != want {
			t.Fatalf("got %d, want %d", seq, want)
		}
	}
}

func TestReplayThenLiveUnderConcurrentAppends(t *testing.T) {
	e := newReplayEnv(t, nil)
	e.ticket(t, "t1")
	e.append(t, "t1", 5)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			e.append(t, "t1", 1)
		}
		close(stop)
	}()
	c, id := subscribeWith(t, e, api.EventsSubscribeParams{Since: map[string]int{"t1": 0}})
	<-stop
	wg.Wait()
	for want := 1; want <= 205; want++ {
		if _, seq := readEntry(t, c, id); seq != want {
			t.Fatalf("got %d, want %d", seq, want)
		}
	}
}

func TestSubscribeFiltersByTicket(t *testing.T) {
	e := newReplayEnv(t, nil)
	e.ticket(t, "t1")
	e.ticket(t, "t2")
	e.append(t, "t1", 1)
	e.append(t, "t2", 2)
	c, id := subscribeWith(t, e, api.EventsSubscribeParams{Tickets: []string{"t2"}, Since: map[string]int{"t2": 0}})
	e.append(t, "t1", 1)
	e.bus.Publish(api.EventTicketCreated, "t1", "", api.EmptyData{})
	e.append(t, "t2", 1)
	for want := 1; want <= 3; want++ {
		if tk, seq := readEntry(t, c, id); tk != "t2" || seq != want {
			t.Fatalf("got %s/%d, want t2/%d", tk, seq, want)
		}
	}
}

func TestReconnectingWithTheLastSeqResumesExactly(t *testing.T) {
	e := newReplayEnv(t, nil)
	e.ticket(t, "t1")
	e.append(t, "t1", 2)
	c, id := subscribeWith(t, e, api.EventsSubscribeParams{Since: map[string]int{"t1": 0}})
	last := 0
	for i := 0; i < 2; i++ {
		_, last = readEntry(t, c, id)
	}
	c.conn.Close()
	e.append(t, "t1", 3) // while disconnected
	c2, id2 := subscribeWith(t, e, api.EventsSubscribeParams{Since: map[string]int{"t1": last}})
	e.append(t, "t1", 1)
	for want := 3; want <= 6; want++ {
		if _, seq := readEntry(t, c2, id2); seq != want {
			t.Fatalf("got %d, want %d", seq, want)
		}
	}
}

func TestSubscribeRefusesBadParams(t *testing.T) {
	e := newReplayEnv(t, nil)
	e.ticket(t, "t1")
	for name, tc := range map[string]struct {
		p    api.EventsSubscribeParams
		code api.ErrorCode
	}{
		"unknown ticket": {api.EventsSubscribeParams{Tickets: []string{"ghost"}}, api.CodeNotFound},
		"negative seq":   {api.EventsSubscribeParams{Since: map[string]int{"t1": -1}}, api.CodeInvalid},
		"bad id":         {api.EventsSubscribeParams{Tickets: []string{"../x"}}, api.CodeInvalid},
	} {
		c := dial(t, e.h.opts.SocketPath)
		c.hello(t)
		id := c.send(t, api.MethodEventsSubscribe, tc.p)
		r, err := c.dec.ReadResponse()
		if err != nil || r.ID != id || r.Err() == nil {
			t.Fatalf("%s: %+v, %v", name, r, err)
		}
		if ae, ok := r.Err().(*api.Error); !ok || ae.Code != tc.code {
			t.Errorf("%s: %v, want %s", name, r.Err(), tc.code)
		}
	}
}
