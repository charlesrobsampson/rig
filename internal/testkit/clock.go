package testkit

import (
	"sort"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/clock"
)

// FakeClock only moves when Advance is called, firing due timers in deadline order.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	timers []*fakeTimer
}

var _ clock.Clock = (*FakeClock)(nil)

func NewFakeClock(start time.Time) *FakeClock { return &FakeClock{now: start} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time { return c.NewTimer(d).C() }

func (c *FakeClock) NewTimer(d time.Duration) clock.Timer {
	return c.schedule(&fakeTimer{ch: make(chan time.Time, 1)}, d)
}

func (c *FakeClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	return c.schedule(&fakeTimer{fn: f}, d)
}

func (c *FakeClock) NewTicker(d time.Duration) clock.Ticker {
	return fakeTicker{c.schedule(&fakeTimer{ch: make(chan time.Time, 1), period: d}, d)}
}

func (c *FakeClock) schedule(t *fakeTimer, d time.Duration) *fakeTimer {
	t.clock = c
	c.mu.Lock()
	defer c.mu.Unlock()
	c.arm(t, d)
	return t
}

// arm needs c.mu held.
func (c *FakeClock) arm(t *fakeTimer, d time.Duration) {
	c.seq++
	t.when = c.now.Add(d)
	t.seq = c.seq
	if !t.pending {
		t.pending = true
		c.timers = append(c.timers, t)
	}
}

// Advance moves time forward, firing everything that comes due on the way. A callback
// may schedule more work; anything due before the new time still fires in this call.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()

	for {
		c.mu.Lock()
		next := c.nextDue(target)
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.now = next.when
		at := next.when
		if next.period > 0 {
			c.arm(next, next.period)
		} else {
			c.remove(next)
		}
		c.mu.Unlock()
		next.fire(at)
	}
}

// nextDue needs c.mu held.
func (c *FakeClock) nextDue(target time.Time) *fakeTimer {
	sort.Slice(c.timers, func(i, j int) bool {
		a, b := c.timers[i], c.timers[j]
		if !a.when.Equal(b.when) {
			return a.when.Before(b.when)
		}
		return a.seq < b.seq
	})
	if len(c.timers) == 0 || c.timers[0].when.After(target) {
		return nil
	}
	return c.timers[0]
}

// remove needs c.mu held.
func (c *FakeClock) remove(t *fakeTimer) {
	t.pending = false
	for i, other := range c.timers {
		if other == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			return
		}
	}
}

type fakeTimer struct {
	clock   *FakeClock
	when    time.Time
	seq     int
	pending bool
	period  time.Duration
	ch      chan time.Time
	fn      func()
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) fire(at time.Time) {
	if t.fn != nil {
		t.fn()
		return
	}
	select {
	case t.ch <- at:
	default: // a slow receiver misses ticks, like the real ticker
	}
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.pending
	t.clock.remove(t)
	return was
}

type fakeTicker struct{ t *fakeTimer }

func (k fakeTicker) C() <-chan time.Time { return k.t.ch }
func (k fakeTicker) Stop()               { k.t.Stop() }

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.pending
	t.clock.arm(t, d)
	return was
}
