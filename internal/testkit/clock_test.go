package testkit_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type timerSpec struct {
	name  string
	after time.Duration
	stop  bool
}

func TestFakeClockFiresCallbacks(t *testing.T) {
	tests := []struct {
		name     string
		timers   []timerSpec
		advances []time.Duration
		fired    [][]string // names newly fired after each advance
	}{
		{
			name:     "fires only after enough time has passed",
			timers:   []timerSpec{{name: "a", after: 10 * time.Second}},
			advances: []time.Duration{9 * time.Second, time.Second},
			fired:    [][]string{nil, {"a"}},
		},
		{
			name:     "fires in deadline order regardless of creation order",
			timers:   []timerSpec{{name: "a", after: 30 * time.Second}, {name: "b", after: 10 * time.Second}, {name: "c", after: 20 * time.Second}},
			advances: []time.Duration{time.Minute},
			fired:    [][]string{{"b", "c", "a"}},
		},
		{
			name:     "equal deadlines fire in creation order",
			timers:   []timerSpec{{name: "a", after: 10 * time.Second}, {name: "b", after: 10 * time.Second}},
			advances: []time.Duration{10 * time.Second},
			fired:    [][]string{{"a", "b"}},
		},
		{
			name:     "stopped timer never fires",
			timers:   []timerSpec{{name: "a", after: 10 * time.Second, stop: true}, {name: "b", after: 10 * time.Second}},
			advances: []time.Duration{time.Minute},
			fired:    [][]string{{"b"}},
		},
		{
			name:     "timer fires once",
			timers:   []timerSpec{{name: "a", after: time.Second}},
			advances: []time.Duration{time.Second, time.Hour},
			fired:    [][]string{{"a"}, nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testkit.NewFakeClock(epoch)
			var log []string
			for _, spec := range tt.timers {
				timer := c.AfterFunc(spec.after, func() { log = append(log, spec.name) })
				if spec.stop {
					timer.Stop()
				}
			}
			seen := 0
			for i, d := range tt.advances {
				c.Advance(d)
				got := log[seen:]
				seen = len(log)
				if len(got) != len(tt.fired[i]) || (len(got) > 0 && !reflect.DeepEqual(got, tt.fired[i])) {
					t.Errorf("after advance %d: fired %v, want %v", i, got, tt.fired[i])
				}
			}
		})
	}
}

func TestFakeClockNowOnlyMovesWithAdvance(t *testing.T) {
	c := testkit.NewFakeClock(epoch)
	if !c.Now().Equal(epoch) {
		t.Fatalf("Now = %v, want %v", c.Now(), epoch)
	}
	c.Advance(90 * time.Minute)
	if want := epoch.Add(90 * time.Minute); !c.Now().Equal(want) {
		t.Fatalf("Now = %v, want %v", c.Now(), want)
	}
}

func TestFakeClockTimerChannel(t *testing.T) {
	c := testkit.NewFakeClock(epoch)
	timer := c.NewTimer(10 * time.Second)

	c.Advance(5 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("timer fired early")
	default:
	}

	c.Advance(10 * time.Second)
	select {
	case at := <-timer.C():
		if want := epoch.Add(10 * time.Second); !at.Equal(want) {
			t.Errorf("fired at %v, want its deadline %v", at, want)
		}
	default:
		t.Fatal("timer did not fire")
	}
}

func TestFakeClockAfter(t *testing.T) {
	c := testkit.NewFakeClock(epoch)
	ch := c.After(time.Minute)
	c.Advance(time.Minute)
	select {
	case <-ch:
	default:
		t.Fatal("After channel did not fire")
	}
}

func TestFakeClockStopAndReset(t *testing.T) {
	tests := []struct {
		name      string
		act       func(c *testkit.FakeClock, timer clock.Timer)
		advance   time.Duration
		wantFired bool
	}{
		{
			name: "reset pushes the deadline out",
			act: func(c *testkit.FakeClock, timer clock.Timer) {
				c.Advance(5 * time.Second)
				timer.Reset(10 * time.Second)
			},
			advance:   9 * time.Second,
			wantFired: false,
		},
		{
			name: "reset deadline is honored",
			act: func(c *testkit.FakeClock, timer clock.Timer) {
				c.Advance(5 * time.Second)
				timer.Reset(10 * time.Second)
			},
			advance:   10 * time.Second,
			wantFired: true,
		},
		{
			name: "stopped timer can be reset and fires",
			act: func(_ *testkit.FakeClock, timer clock.Timer) {
				timer.Stop()
				timer.Reset(time.Second)
			},
			advance:   time.Second,
			wantFired: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testkit.NewFakeClock(epoch)
			fired := false
			timer := c.AfterFunc(10*time.Second, func() { fired = true })
			tt.act(c, timer)
			c.Advance(tt.advance)
			if fired != tt.wantFired {
				t.Errorf("fired = %v, want %v", fired, tt.wantFired)
			}
		})
	}
}

func TestFakeClockStopReportsWhetherItStoppedATimer(t *testing.T) {
	c := testkit.NewFakeClock(epoch)
	timer := c.AfterFunc(time.Second, func() {})
	if !timer.Stop() {
		t.Error("Stop on a pending timer returned false")
	}
	if timer.Stop() {
		t.Error("Stop on a stopped timer returned true")
	}
	fired := c.AfterFunc(time.Second, func() {})
	c.Advance(time.Second)
	if fired.Stop() {
		t.Error("Stop on a fired timer returned true")
	}
}

func TestFakeClockTicker(t *testing.T) {
	tests := []struct {
		name      string
		intervals int
	}{
		{"one interval", 1},
		{"five intervals", 5},
		{"fifty intervals", 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testkit.NewFakeClock(epoch)
			ticker := c.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for i := 1; i <= tt.intervals; i++ {
				c.Advance(10 * time.Second)
				select {
				case at := <-ticker.C():
					if want := epoch.Add(time.Duration(i) * 10 * time.Second); !at.Equal(want) {
						t.Fatalf("tick %d at %v, want %v", i, at, want)
					}
				default:
					t.Fatalf("tick %d did not fire", i)
				}
			}
		})
	}
}

func TestFakeClockStoppedTickerStaysQuiet(t *testing.T) {
	c := testkit.NewFakeClock(epoch)
	ticker := c.NewTicker(time.Second)
	ticker.Stop()
	c.Advance(time.Minute)
	select {
	case <-ticker.C():
		t.Fatal("stopped ticker fired")
	default:
	}
}

func TestFakeClockCallbackMayScheduleMoreWork(t *testing.T) {
	c := testkit.NewFakeClock(epoch)
	var log []string
	c.AfterFunc(10*time.Second, func() {
		log = append(log, "first")
		c.AfterFunc(5*time.Second, func() { log = append(log, "second") })
	})
	c.Advance(time.Minute)
	if want := []string{"first", "second"}; !reflect.DeepEqual(log, want) {
		t.Errorf("log = %v, want %v (follow-up due inside the same Advance)", log, want)
	}
}
