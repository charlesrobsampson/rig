package ports_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/docker/ports"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var ctx = context.Background()

type fixture struct {
	t      *testing.T
	dir    string
	states *state.Manager
	busy   map[int]bool
	mu     sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, dir: testkit.RealTempDir(t), states: state.NewManager(testkit.NewFakeClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))), busy: map[int]bool{}}
}

func (f *fixture) ticket(id string, services ...string) {
	f.t.Helper()
	os.MkdirAll(filepath.Join(f.dir, id), 0o700)
	st := state.State{TicketID: id, Services: map[string]state.ServiceState{}}
	for _, s := range services {
		st.Services[s] = state.ServiceState{}
	}
	if err := f.states.Create(filepath.Join(f.dir, id, "state.json"), st); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) allocator(min, max int) *ports.Allocator {
	return ports.New(ports.Options{Dir: f.dir, States: f.states, Min: min, Max: max, Probe: func(p int) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return !f.busy[p]
	}})
}

func (f *fixture) setBusy(p int, v bool) { f.mu.Lock(); f.busy[p] = v; f.mu.Unlock() }

func (f *fixture) recorded(ticket, service string) int {
	f.t.Helper()
	st, err := f.states.Load(filepath.Join(f.dir, ticket, "state.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Services[service].HostPort
}

func (f *fixture) allocate(a *ports.Allocator, ticket, service string) int {
	f.t.Helper()
	p, err := a.Allocate(ctx, ticket, service)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func TestTicketsNeverShareAPort(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api", "web")
	f.ticket("two", "api", "web") // the same service names as the first
	a := f.allocator(41000, 41999)
	seen := map[int]string{}
	for _, tk := range []string{"one", "two"} {
		for _, svc := range []string{"api", "web"} {
			p := f.allocate(a, tk, svc)
			if prev, dup := seen[p]; dup {
				t.Errorf("%s/%s got port %d, already held by %s", tk, svc, p, prev)
			}
			seen[p] = tk + "/" + svc
			if got := f.recorded(tk, svc); got != p {
				t.Errorf("%s/%s recorded %d, returned %d", tk, svc, got, p)
			}
		}
	}
	for p := range seen {
		if p < 41000 || p > 41999 {
			t.Errorf("port %d is outside the range", p)
		}
	}
}

func TestAPortInUseOnTheMachineIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	f.setBusy(41000, true)
	f.setBusy(41001, true)
	if p := f.allocate(f.allocator(41000, 41999), "one", "api"); p != 41002 {
		t.Errorf("port = %d, want 41002 (41000 and 41001 are busy)", p)
	}
}

func TestARecordedPortIsReusedAfterARestart(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	first := f.allocate(f.allocator(41000, 41999), "one", "api")
	before, _ := os.Stat(filepath.Join(f.dir, "one", "state.json"))

	restarted := f.allocator(41000, 41999)
	if got := f.allocate(restarted, "one", "api"); got != first {
		t.Errorf("after a restart the port is %d, want %d", got, first)
	}
	after, _ := os.Stat(filepath.Join(f.dir, "one", "state.json"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("reusing a recorded port rewrote state.json")
	}
}

func TestAPortTakenByAnotherProcessIsReplacedAndRecorded(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	a := f.allocator(41000, 41999)
	first := f.allocate(a, "one", "api")
	f.setBusy(first, true) // a foreign process grabbed it while rig was down
	second := f.allocate(f.allocator(41000, 41999), "one", "api")
	if second == first || f.recorded("one", "api") != second {
		t.Errorf("port = %d (recorded %d), want a new one different from %d", second, f.recorded("one", "api"), first)
	}
}

func TestAProbeThatKnowsTheOwnersPortsKeepsThem(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	first := f.allocate(f.allocator(41000, 41999), "one", "api")
	f.setBusy(first, true) // held, but by the service's own running container
	own := ports.New(ports.Options{Dir: f.dir, States: f.states, Min: 41000, Max: 41999, Probe: func(p int) bool { return p == first || !f.busy[p] }})
	if got := f.allocate(own, "one", "api"); got != first {
		t.Errorf("port = %d, want the container's own port %d kept", got, first)
	}
}

func TestARecordedPortThatAnotherTicketHoldsOrThatLeftTheRangeIsReplaced(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	f.ticket("two", "api")
	a := f.allocator(41000, 41999)
	p := f.allocate(a, "one", "api")
	// state.json of ticket two was edited by hand to claim the same port
	f.states.Update(filepath.Join(f.dir, "two", "state.json"), func(s *state.State) error {
		ss := s.Services["api"]
		ss.HostPort = p
		s.Services["api"] = ss
		return nil
	})
	if got := f.allocate(a, "two", "api"); got == p {
		t.Errorf("two got %d, which ticket one holds", got)
	}

	f.ticket("three", "api")
	f.states.Update(filepath.Join(f.dir, "three", "state.json"), func(s *state.State) error {
		ss := s.Services["api"]
		ss.HostPort = 3000
		s.Services["api"] = ss
		return nil
	})
	if got := f.allocate(a, "three", "api"); got < 41000 || got > 41999 {
		t.Errorf("three kept %d, outside the configured range", got)
	}
}

func TestAnExhaustedRangeIsAnError(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "a", "b")
	f.ticket("two", "a")
	a := f.allocator(41000, 41001)
	f.allocate(a, "one", "a")
	f.allocate(a, "one", "b")
	if _, err := a.Allocate(ctx, "two", "a"); !errors.Is(err, ports.ErrExhausted) {
		t.Errorf("error = %v, want ErrExhausted", err)
	}
	if f.recorded("two", "a") != 0 {
		t.Error("a failed allocation recorded a port")
	}
}

func TestReleaseFreesThePortForOtherTickets(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	f.ticket("two", "api")
	a := f.allocator(41000, 41000) // a single port
	p := f.allocate(a, "one", "api")
	if _, err := a.Allocate(ctx, "two", "api"); !errors.Is(err, ports.ErrExhausted) {
		t.Fatalf("two should not get ticket one's port: %v", err)
	}
	if err := a.Release("one", "api"); err != nil {
		t.Fatal(err)
	}
	if f.recorded("one", "api") != 0 {
		t.Error("the port is still recorded after Release")
	}
	if got := f.allocate(a, "two", "api"); got != p {
		t.Errorf("two got %d, want the released %d", got, p)
	}
	if err := a.Release("one", "api"); err != nil {
		t.Errorf("releasing twice: %v", err)
	}
}

func TestConcurrentAllocationsAreDistinct(t *testing.T) {
	f := newFixture(t)
	for _, tk := range []string{"a", "b", "c", "d", "e"} {
		f.ticket(tk, "api", "web", "db", "cache")
	}
	a := f.allocator(41000, 41999)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var got []int
	for _, tk := range []string{"a", "b", "c", "d", "e"} {
		for _, svc := range []string{"api", "web", "db", "cache"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := a.Allocate(ctx, tk, svc)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				got = append(got, p)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	sort.Ints(got)
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("port %d was handed out twice: %v", got[i], got)
		}
	}
	if len(got) != 20 {
		t.Errorf("got %d ports, want 20", len(got))
	}
}

func TestUnknownTicketsAndServicesAreErrors(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	a := f.allocator(41000, 41999)
	for _, tc := range [][2]string{{"ghost", "api"}, {"one", "nope"}, {"../x", "api"}} {
		if _, err := a.Allocate(ctx, tc[0], tc[1]); err == nil {
			t.Errorf("Allocate(%s, %s) succeeded", tc[0], tc[1])
		}
	}
}

func TestABrokenTicketDoesNotBlockOthers(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	os.MkdirAll(filepath.Join(f.dir, "broken"), 0o700)
	os.WriteFile(filepath.Join(f.dir, "broken", "state.json"), []byte("{not json"), 0o600)
	if _, err := f.allocator(41000, 41999).Allocate(ctx, "one", "api"); err != nil {
		t.Errorf("a broken neighbour made the allocation fail: %v", err)
	}
}

func TestTheDefaultProbeSeesARealListener(t *testing.T) {
	f := newFixture(t)
	f.ticket("one", "api")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	a := ports.New(ports.Options{Dir: f.dir, States: f.states, Min: port, Max: port})
	if _, err := a.Allocate(ctx, "one", "api"); !errors.Is(err, ports.ErrExhausted) {
		t.Errorf("a port with a listener was handed out: %v", err)
	}
	l.Close()
	if p, err := a.Allocate(ctx, "one", "api"); err != nil || p != port {
		t.Errorf("after the listener closed: %d, %v", p, err)
	}
}
