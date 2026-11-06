// Package clock is the only source of time for rig logic, so tests can control it.
package clock

import "time"

type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
	AfterFunc(d time.Duration, f func()) Timer
	NewTicker(d time.Duration) Ticker
}

// Timer's channel is nil for timers made by AfterFunc.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type real struct{}

func Real() Clock { return real{} }

func (real) Now() time.Time                         { return time.Now() }
func (real) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (real) NewTimer(d time.Duration) Timer         { return realTimer{time.NewTimer(d)} }
func (real) NewTicker(d time.Duration) Ticker       { return realTicker{time.NewTicker(d)} }
func (real) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{time.AfterFunc(d, f)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
