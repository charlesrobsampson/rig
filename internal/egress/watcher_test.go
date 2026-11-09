package egress_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/egress"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type watchFixture struct {
	t     *testing.T
	d     *testkit.FakeDocker
	clock *testkit.FakeClock
	bus   *events.Bus
	sub   *events.Subscription
	stop  func()
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	f := &watchFixture{t: t, d: testkit.NewFakeDocker(), clock: testkit.NewFakeClock(epoch)}
	f.d.AddImage("proxy")
	f.bus = events.NewBus(f.clock, 0)
	f.sub = f.bus.Subscribe()
	<-f.sub.Events()
	t.Cleanup(f.sub.Close)
	return f
}

func (f *watchFixture) start() {
	f.t.Helper()
	ctx, cancel := contextWithCancel()
	done := make(chan struct{})
	w := &egress.Watcher{Docker: f.d, Bus: f.bus, Clock: f.clock, Interval: time.Second}
	go func() { w.Run(ctx); close(done) }()
	f.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.t.Error("the watcher did not stop")
		}
	}
	f.t.Cleanup(func() { cancel() })
}

func (f *watchFixture) proxy(ticket string) string {
	f.t.Helper()
	c, err := f.d.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-" + ticket + "-proxy", Image: "proxy", Labels: docker.Labels(ticket, "", docker.RoleProxy)})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.d.StartContainer(ctx, c.ID); err != nil {
		f.t.Fatal(err)
	}
	return c.ID
}

func (f *watchFixture) logLine(id string, e egress.Entry) {
	raw, _ := json.Marshal(e)
	f.d.Print(id, string(raw))
}

// next waits for an egress.denied event, advancing the fake clock so the watcher scans.
func (f *watchFixture) next() (api.Event, api.EgressDeniedData) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-f.sub.Events():
			if ev.Type == api.EventEgressDenied {
				var d api.EgressDeniedData
				json.Unmarshal(ev.Data, &d)
				return ev, d
			}
		default:
			f.clock.Advance(time.Second)
			time.Sleep(2 * time.Millisecond)
		}
	}
	f.t.Fatal("no egress.denied event arrived")
	return api.Event{}, api.EgressDeniedData{}
}

func (f *watchFixture) noMore() {
	f.t.Helper()
	for range 20 {
		f.clock.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case ev := <-f.sub.Events():
		f.t.Errorf("unexpected event %s %s", ev.Type, ev.Data)
	default:
	}
}

func TestDeniedConnectionsBecomeEvents(t *testing.T) {
	f := newWatchFixture(t)
	id := f.proxy("demo")
	f.start()
	defer f.stop()
	now := epoch.Add(time.Minute)
	f.logLine(id, egress.Entry{TS: now, Action: "start", Host: ":3128", Reason: "api.anthropic.com"})
	f.logLine(id, egress.Entry{TS: now, Action: "allow", Host: "api.anthropic.com:443"})
	f.logLine(id, egress.Entry{TS: now.Add(time.Second), Action: "deny", Host: "example.com:443", Reason: "not in allowlist"})
	ev, d := f.next()
	if ev.Ticket != "demo" || d.Host != "example.com" || d.Port != 443 || d.Reason != "not in allowlist" {
		t.Errorf("event = %+v %+v", ev, d)
	}
	f.logLine(id, egress.Entry{TS: now.Add(2 * time.Second), Action: "deny", Host: "10.0.0.5:8080", Reason: "plain http not allowed"})
	if _, d := f.next(); d.Host != "10.0.0.5" || d.Port != 8080 || d.Reason != "plain http not allowed" {
		t.Errorf("second event = %+v", d)
	}
	f.noMore() // the start and allow lines announced nothing
}

func TestHistoryAndNoiseAreNotAnnounced(t *testing.T) {
	f := newWatchFixture(t)
	id := f.proxy("demo")
	f.logLine(id, egress.Entry{TS: epoch.Add(-time.Hour), Action: "deny", Host: "old.com:443", Reason: "from before the daemon started"})
	f.start()
	defer f.stop()
	f.d.Print(id, "this line is not JSON")
	f.d.Print(id, `{"action":"deny"`)
	f.d.Print(id, `{"ts":"not a time","action":"deny","host":"x:1"}`)
	f.logLine(id, egress.Entry{TS: epoch.Add(time.Minute), Action: "deny", Host: "fresh.com:443", Reason: "not in allowlist"})
	if _, d := f.next(); d.Host != "fresh.com" {
		t.Errorf("event = %+v; the old entry and the malformed lines must be skipped", d)
	}
	f.noMore()
}

func TestEachTicketsProxyIsFollowedAndAttributed(t *testing.T) {
	f := newWatchFixture(t)
	a, b := f.proxy("alpha"), f.proxy("beta")
	f.start()
	defer f.stop()
	now := epoch.Add(time.Minute)
	f.logLine(a, egress.Entry{TS: now, Action: "deny", Host: "a.example:443", Reason: "not in allowlist"})
	f.logLine(b, egress.Entry{TS: now.Add(time.Second), Action: "deny", Host: "b.example:443", Reason: "not in allowlist"})
	got := map[string]string{}
	for range 2 {
		ev, d := f.next()
		got[ev.Ticket] = d.Host
	}
	if got["alpha"] != "a.example" || got["beta"] != "b.example" {
		t.Errorf("events by ticket = %v", got)
	}
}

func TestAProxyThatStartsLaterIsPickedUp(t *testing.T) {
	f := newWatchFixture(t)
	f.start()
	defer f.stop()
	for range 3 { // the watcher runs with nothing to watch
		f.clock.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	id := f.proxy("late")
	f.logLine(id, egress.Entry{TS: epoch.Add(time.Hour), Action: "deny", Host: "x.example:443", Reason: "not in allowlist"})
	if ev, d := f.next(); ev.Ticket != "late" || d.Host != "x.example" {
		t.Errorf("event = %+v %+v", ev, d)
	}
}

func TestARestartedFollowerDoesNotRepeatEvents(t *testing.T) {
	f := newWatchFixture(t)
	id := f.proxy("demo")
	f.start()
	defer f.stop()
	f.logLine(id, egress.Entry{TS: epoch.Add(time.Minute), Action: "deny", Host: "x.example:443", Reason: "not in allowlist"})
	f.next()
	f.d.Exit(id, 1) // the proxy dies; its logs remain
	f.d.StartContainer(ctx, id)
	f.noMore() // following again replays the log, but nothing new happened
	f.logLine(id, egress.Entry{TS: epoch.Add(2 * time.Minute), Action: "deny", Host: "y.example:443", Reason: "not in allowlist"})
	if _, d := f.next(); d.Host != "y.example" {
		t.Errorf("event = %+v", d)
	}
}

func TestDockerBeingDownIsSurvived(t *testing.T) {
	f := newWatchFixture(t)
	id := f.proxy("demo")
	f.d.Down(fmt.Errorf("stopped"))
	f.start()
	defer f.stop()
	for range 3 {
		f.clock.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	f.d.Up()
	f.logLine(id, egress.Entry{TS: epoch.Add(time.Minute), Action: "deny", Host: "x.example:443", Reason: "not in allowlist"})
	if _, d := f.next(); d.Host != "x.example" {
		t.Errorf("event = %+v", d)
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
