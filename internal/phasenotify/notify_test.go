package phasenotify_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/phasenotify"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

type attempt struct{ service, text, from string }

// fakeSender refuses a service with the error queued for it, then delivers.
type fakeSender struct {
	mu        sync.Mutex
	refuse    map[string][]error
	attempts  []attempt
	delivered []attempt
}

func (f *fakeSender) Send(_ context.Context, p api.AgentMessageParams, from string) (api.AgentMessageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := attempt{p.Service, p.Text, from}
	f.attempts = append(f.attempts, a)
	if errs := f.refuse[p.Service]; len(errs) > 0 {
		f.refuse[p.Service] = errs[1:]
		return api.AgentMessageResult{}, errs[0]
	}
	f.delivered = append(f.delivered, a)
	return api.AgentMessageResult{Service: p.Service}, nil
}

func (f *fakeSender) got() []attempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]attempt(nil), f.delivered...)
	sort.Slice(out, func(i, j int) bool { return out[i].service < out[j].service })
	return out
}

func (f *fakeSender) tries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

func blocked() error {
	return &api.Error{Code: api.CodeBlocked, Message: "waiting on a permission prompt"}
}

func setup(t *testing.T, s *fakeSender) (*phasenotify.Notifier, *testkit.FakeClock) {
	t.Helper()
	c := testkit.NewFakeClock(epoch)
	n := phasenotify.New(phasenotify.Options{Sender: s, Clock: c, Retry: 10 * time.Second, MaxRetry: 40 * time.Second})
	t.Cleanup(n.Close)
	return n, c
}

func TestEachNoticeIsTypedIntoItsAgentAsRig(t *testing.T) {
	s := &fakeSender{}
	n, _ := setup(t, s)
	n.PhaseChanged("demo", map[string]string{"api": "to implement (api)", "web": "to implement (web)"})
	n.Wait()
	got := s.got()
	if len(got) != 2 || got[0] != (attempt{"api", "to implement (api)", "rig"}) || got[1] != (attempt{"web", "to implement (web)", "rig"}) {
		t.Errorf("delivered = %+v", got)
	}
}

func TestABlockedAgentGetsTheNoticeOnceItIsFree(t *testing.T) {
	s := &fakeSender{refuse: map[string][]error{"api": {blocked(), blocked(), blocked()}}}
	n, c := setup(t, s)
	n.PhaseChanged("demo", map[string]string{"api": "to implement"})
	n.Wait()
	if len(s.got()) != 0 || s.tries() != 1 {
		t.Fatalf("delivered %+v in %d tries while blocked", s.got(), s.tries())
	}
	// Waits grow: 10s, then 20s, then 40s, and never more than MaxRetry.
	for i, wait := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		c.Advance(wait - time.Second)
		n.Wait()
		if s.tries() != i+1 {
			t.Fatalf("retry %d came before %s", i+1, wait)
		}
		c.Advance(time.Second)
		n.Wait()
		if s.tries() != i+2 {
			t.Fatalf("retry %d did not come after %s (tries %d)", i+1, wait, s.tries())
		}
	}
	if got := s.got(); len(got) != 1 || got[0].text != "to implement" {
		t.Errorf("delivered = %+v", got)
	}
	c.Advance(time.Hour)
	n.Wait()
	if s.tries() != 4 {
		t.Errorf("tries = %d after delivery, want no more", s.tries())
	}
}

func TestAStartingAgentIsRetriedToo(t *testing.T) {
	s := &fakeSender{refuse: map[string][]error{"api": {&api.Error{Code: api.CodeUnavailable, Message: "not ready"}, context.DeadlineExceeded}}}
	n, c := setup(t, s)
	n.PhaseChanged("demo", map[string]string{"api": "to test"})
	n.Wait()
	c.Advance(10 * time.Second)
	n.Wait()
	c.Advance(20 * time.Second)
	n.Wait()
	if got := s.got(); len(got) != 1 {
		t.Errorf("delivered = %+v after a not-ready agent and a timeout", got)
	}
}

func TestANewerMoveReplacesAWaitingNotice(t *testing.T) {
	s := &fakeSender{refuse: map[string][]error{"api": {blocked()}}}
	n, c := setup(t, s)
	n.PhaseChanged("demo", map[string]string{"api": "to implement"})
	n.Wait()
	n.PhaseChanged("demo", map[string]string{"api": "back to plan"})
	n.Wait()
	c.Advance(time.Hour)
	n.Wait()
	if got := s.got(); len(got) != 1 || got[0].text != "back to plan" {
		t.Errorf("delivered = %+v, want only the newer notice", got)
	}
	if s.tries() != 2 {
		t.Errorf("tries = %d; the replaced notice was retried", s.tries())
	}
}

func TestAnAgentThatIsGoneIsNotRetried(t *testing.T) {
	s := &fakeSender{refuse: map[string][]error{"api": {&api.Error{Code: api.CodeNotRunning, Message: "exited"}}}}
	n, c := setup(t, s)
	n.PhaseChanged("demo", map[string]string{"api": "to implement"})
	n.Wait()
	c.Advance(time.Hour)
	n.Wait()
	if s.tries() != 1 || len(s.got()) != 0 {
		t.Errorf("tries = %d, delivered %+v", s.tries(), s.got())
	}
}

func TestClosingDropsWaitingNotices(t *testing.T) {
	s := &fakeSender{refuse: map[string][]error{"api": {blocked()}}}
	n, c := setup(t, s)
	n.PhaseChanged("demo", map[string]string{"api": "to implement"})
	n.Wait()
	n.Close()
	c.Advance(time.Hour)
	n.PhaseChanged("demo", map[string]string{"api": "to test"})
	n.Wait()
	if s.tries() != 1 {
		t.Errorf("tries = %d after Close", s.tries())
	}
}
