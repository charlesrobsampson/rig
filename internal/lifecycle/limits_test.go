package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// capFixture is a fixture whose daemon allows at most limit active tickets (0: no limit).
func capFixture(t *testing.T, limit int) *fixture {
	t.Helper()
	f := newFixture(t, domain.SandboxHost)
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch),
		Global: config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxHost, BranchPrefix: "rig/"}, Limits: config.Limits{MaxActiveTickets: limit}},
	})
	return f
}

func (f *fixture) startHost(id string) (api.TicketStartResult, error) {
	return f.lc.Start(ctx, api.TicketStartParams{ID: id, Sandbox: domain.SandboxHost})
}

func TestStartIsRefusedAtTheCapAndSaysWhatToDo(t *testing.T) {
	f := capFixture(t, 2)
	for _, id := range []string{"a", "b", "c"} {
		f.create(id, testkit.NewRepo(t))
	}
	for _, id := range []string{"a", "b"} {
		if _, err := f.startHost(id); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}
	_, err := f.startHost("c")
	if apiCode(t, err) != api.CodeBlocked {
		t.Fatalf("error = %v, want blocked", err)
	}
	apiErr := err.(*api.Error)
	for _, want := range []string{`"c"`, "2 tickets are already active", "limit is 2", "limits.max_active_tickets", "rig stop"} {
		if !strings.Contains(apiErr.Message, want) {
			t.Errorf("message lacks %q: %s", want, apiErr.Message)
		}
	}
	if len(apiErr.Details) != 1 || apiErr.Details[0] != "active: a, b" {
		t.Errorf("details = %v", apiErr.Details)
	}
	if exists(filepath.Join(f.dir, "c", "svc1")) {
		t.Error("a refused start provisioned a checkout")
	}
	if st := f.state("c"); st.Active() {
		t.Error("a refused start left the ticket active")
	}
}

func TestStoppingFreesASlot(t *testing.T) {
	f := capFixture(t, 1)
	f.create("a", testkit.NewRepo(t))
	f.create("b", testkit.NewRepo(t))
	f.startHost("a")
	if _, err := f.startHost("b"); apiCode(t, err) != api.CodeBlocked {
		t.Fatalf("b = %v", err)
	}
	f.mustStop("a", false, false)
	if _, err := f.startHost("b"); err != nil {
		t.Errorf("after stopping a: %v", err)
	}
}

func TestAnActiveTicketCanAlwaysBeStartedAgain(t *testing.T) {
	f := capFixture(t, 1)
	f.create("a", testkit.NewRepo(t))
	f.startHost("a")
	if _, err := f.startHost("a"); err != nil {
		t.Errorf("restarting the one active ticket at the cap: %v", err)
	}
}

func TestDoneTicketsAndNoLimitDoNotCount(t *testing.T) {
	f := capFixture(t, 1)
	f.create("old", testkit.NewRepo(t))
	f.create("new", testkit.NewRepo(t))
	f.startHost("old")
	f.states.Update(filepath.Join(f.dir, "old", "state.json"), func(s *state.State) error { s.Phase = domain.PhaseDone; return nil })
	if _, err := f.startHost("new"); err != nil {
		t.Errorf("a done ticket took the slot: %v", err)
	}

	g := capFixture(t, 0)
	for _, id := range []string{"a", "b", "c", "d"} {
		g.create(id, testkit.NewRepo(t))
		if _, err := g.startHost(id); err != nil {
			t.Errorf("with no limit, %s: %v", id, err)
		}
	}
}

func TestADryRunNeitherNeedsNorTakesASlot(t *testing.T) {
	f := capFixture(t, 1)
	f.create("a", testkit.NewRepo(t))
	f.create("b", testkit.NewRepo(t))
	f.startHost("a")
	if _, err := f.lc.Start(ctx, api.TicketStartParams{ID: "b", Sandbox: domain.SandboxHost, DryRun: true}); err != nil {
		t.Errorf("a dry run was refused at the cap: %v", err)
	}
}

func TestAStartThatFailsDoesNotKeepItsSlot(t *testing.T) {
	f := capFixture(t, 1)
	path := f.create("bad", testkit.NewRepo(t))
	f.create("good", testkit.NewRepo(t))
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), []byte(strings.Replace(string(raw), "repo: ", "repo: /does/not/exist#", 1)), 0o600)
	if _, err := f.startHost("bad"); err == nil {
		t.Skip("the broken repository was accepted")
	}
	if _, err := f.startHost("good"); err != nil {
		t.Errorf("a failed start held the only slot: %v", err)
	}
}

func TestTwoStartsAtOnceCannotBothTakeTheLastSlot(t *testing.T) {
	f := capFixture(t, 1)
	const n = 6
	for i := 0; i < n; i++ {
		f.create("t"+string(rune('a'+i)), testkit.NewRepo(t))
	}
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = f.startHost("t" + string(rune('a'+i)))
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("%d starts succeeded with a limit of 1: %v", succeeded, results)
	}
}

func TestLoweringTheLimitNeverStrandsTicketsThatAreAlreadyRunning(t *testing.T) {
	f := capFixture(t, 0)
	for _, id := range []string{"a", "b"} {
		f.create(id, testkit.NewRepo(t))
		f.startHost(id)
	}
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch),
		Global: config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxHost, BranchPrefix: "rig/"}, Limits: config.Limits{MaxActiveTickets: 1}}, // two are active, one is allowed
	})
	for _, id := range []string{"a", "b"} {
		if _, err := f.startHost(id); err != nil {
			t.Errorf("restarting active ticket %s under a lowered limit: %v", id, err)
		}
	}
	f.create("c", testkit.NewRepo(t))
	if _, err := f.startHost("c"); apiCode(t, err) != api.CodeBlocked {
		t.Errorf("a new ticket under the lowered limit = %v", err)
	}
}
