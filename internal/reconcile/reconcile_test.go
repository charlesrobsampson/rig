package reconcile_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/reconcile"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
var ctx = context.Background()

type fixture struct {
	t      *testing.T
	dir    string
	clock  *testkit.FakeClock
	states *state.Manager
	dk     *testkit.FakeDocker
	term   *testkit.FakeTerminal
	bus    *events.Bus
	sub    *events.Subscription
	r      *reconcile.Reconciler
	busy   map[string]bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := testkit.NewFakeClock(epoch)
	f := &fixture{
		t: t, dir: testkit.RealTempDir(t), clock: clock, states: state.NewManager(clock),
		dk: testkit.NewFakeDocker(), term: testkit.NewFakeTerminal(), bus: events.NewBus(clock, 0), busy: map[string]bool{},
	}
	f.dk.AddImage("img")
	f.sub = f.bus.Subscribe()
	<-f.sub.Events()
	t.Cleanup(f.sub.Close)
	f.r = f.build(f.dk, f.term)
	return f
}

func (f *fixture) build(dk docker.Docker, term terminal.Backend) *reconcile.Reconciler {
	return reconcile.New(reconcile.Options{
		Dir: f.dir, States: f.states, Docker: dk, Backend: term, Bus: f.bus, Clock: f.clock,
		Busy: func(id string) bool { return f.busy[id] }, Interval: time.Minute,
	})
}

// ticket writes a ticket with services api and web and a dependency db.
func (f *fixture) ticket(id string, mutate func(*state.State)) string {
	f.t.Helper()
	dir := filepath.Join(f.dir, id)
	os.MkdirAll(dir, 0o700)
	yaml := "id: " + id + "\ntitle: T\nservices:\n  - {name: api, repo: /repos/api, branch: b/" + id + "}\n  - {name: web, repo: /repos/web, branch: b/" + id + "}\ndependencies:\n  - {name: db, image: postgres:16}\n"
	os.WriteFile(filepath.Join(dir, "ticket.yaml"), []byte(yaml), 0o600)
	st := state.State{TicketID: id, Phase: domain.PhaseImplement, Services: map[string]state.ServiceState{}, Dependencies: map[string]state.DependencyState{}}
	if mutate != nil {
		mutate(&st)
	}
	if err := f.states.Create(filepath.Join(dir, "state.json"), st); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// running is a ticket whose everything is up and recorded: checkouts, network, a service
// container, a dependency container, an agent pane and an agent container for api.
type running struct {
	dir                      string
	network                  docker.NetworkInfo
	svc, dep, agent          docker.ContainerInfo
	pane                     terminal.PaneID
	apiCheckout, webCheckout string
}

func (f *fixture) runningTicket(id string) running {
	f.t.Helper()
	var r running
	dir := filepath.Join(f.dir, id)
	r.apiCheckout, r.webCheckout = filepath.Join(dir, "api"), filepath.Join(dir, "web")
	os.MkdirAll(r.apiCheckout, 0o700)
	os.MkdirAll(r.webCheckout, 0o700)
	var err error
	if r.network, err = f.dk.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-" + id, Labels: docker.Labels(id, "", docker.RoleNetwork)}); err != nil {
		f.t.Fatal(err)
	}
	mk := func(name, service, role string) docker.ContainerInfo {
		c, err := f.dk.CreateContainer(ctx, docker.ContainerSpec{Name: name, Image: "img", Network: r.network.Name, Labels: docker.Labels(id, service, role)})
		if err != nil {
			f.t.Fatal(err)
		}
		f.dk.StartContainer(ctx, c.ID)
		c, _ = f.dk.InspectContainer(ctx, c.ID)
		return c
	}
	r.svc, r.dep, r.agent = mk("rig-"+id+"-api", "api", docker.RoleService), mk("rig-"+id+"-db", "db", docker.RoleDependency), mk("rig-"+id+"-api-agent", "api", docker.RoleAgent)
	if r.pane, err = f.term.Spawn(ctx, terminal.SpawnSpec{Ticket: id, Service: "api", Dir: r.apiCheckout, Command: []string{"agent"}}); err != nil {
		f.t.Fatal(err)
	}
	r.dir = f.ticket(id, func(s *state.State) {
		s.Network = state.Network{Name: r.network.Name, ID: r.network.ID}
		s.Services["api"] = state.ServiceState{Status: domain.ServiceRunning, Checkout: state.Checkout{Path: r.apiCheckout}, ContainerID: r.svc.ID,
			Agent: state.Agent{PaneID: string(r.pane), State: domain.AgentWorking, ContainerID: r.agent.ID}}
		s.Services["web"] = state.ServiceState{Status: domain.ServiceProvisioned, Checkout: state.Checkout{Path: r.webCheckout}}
		s.Dependencies["db"] = state.DependencyState{ContainerID: r.dep.ID, Status: domain.ServiceRunning}
	})
	return r
}

func (f *fixture) run(fix bool) api.ReconcileResult {
	f.t.Helper()
	res, err := f.r.Reconcile(ctx, fix)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func kinds(res api.ReconcileResult) string {
	var out []string
	for _, d := range res.Drift {
		out = append(out, d.Ticket+"/"+d.Service+":"+d.Kind)
	}
	return strings.Join(out, ",")
}

func (f *fixture) reconcileEvents() []api.Event {
	var out []api.Event
	for {
		select {
		case ev := <-f.sub.Events():
			if ev.Type == api.EventReconciled {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

func TestACleanWorldHasNoDrift(t *testing.T) {
	f := newFixture(t)
	if res := f.run(false); len(res.Drift) != 0 || len(res.Notes) != 0 || res.Drift == nil {
		t.Errorf("an empty world: %+v", res)
	}
	f.runningTicket("demo")
	if res := f.run(false); len(res.Drift) != 0 {
		t.Errorf("a consistent running ticket drifts: %s", kinds(res))
	}
	if got := f.r.Drift("demo"); len(got) != 0 {
		t.Errorf("Drift = %v", got)
	}
}

func TestAKilledServiceContainerShowsAsMissing(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	f.dk.RemoveContainer(ctx, r.svc.ID, true)
	res := f.run(false)
	if kinds(res) != "demo/api:service_container_missing" || !strings.Contains(res.Drift[0].Detail, "is gone") || res.Drift[0].Fixable {
		t.Errorf("drift = %+v", res.Drift)
	}
	if got := f.r.Drift("demo"); len(got) != 1 || !strings.HasPrefix(got[0], "api: the service's container") {
		t.Errorf("Drift = %v", got)
	}
}

func TestAContainerThatStoppedIsReportedWithItsState(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	f.dk.StopContainer(ctx, r.svc.ID, time.Second)
	res := f.run(false)
	if kinds(res) != "demo/api:service_container_missing" || !strings.Contains(res.Drift[0].Detail, "is exited") {
		t.Errorf("drift = %+v", res.Drift)
	}
}

func TestOtherMissingThingsAreEachNamed(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	f.dk.RemoveContainer(ctx, r.dep.ID, true)
	f.dk.RemoveContainer(ctx, r.agent.ID, true)
	f.term.Vanish(r.pane)
	os.RemoveAll(r.webCheckout)
	os.RemoveAll(r.apiCheckout)
	f.dk.RemoveContainer(ctx, r.svc.ID, true)
	f.dk.RemoveNetwork(ctx, r.network.Name)
	res := f.run(false)
	got := map[string]bool{}
	for _, d := range res.Drift {
		got[d.Kind+"@"+d.Service] = true
	}
	for _, want := range []string{
		"checkout_missing@api", "checkout_missing@web", "service_container_missing@api", "agent_container_missing@api",
		"agent_pane_missing@api", "dependency_container_missing@db", "network_missing@",
	} {
		if !got[want] {
			t.Errorf("no %s in %s", want, kinds(res))
		}
	}
	for _, d := range res.Drift {
		if d.Fixable {
			t.Errorf("%s is marked fixable: missing things are never repaired automatically", d.Kind)
		}
	}
}

func TestAPaneThatEndedIsDriftToo(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	f.term.Exit(r.pane, 1)
	if res := f.run(false); kinds(res) != "demo/api:agent_pane_missing" || !strings.Contains(res.Drift[0].Detail, "has ended") {
		t.Errorf("drift = %+v", res.Drift)
	}
}

func TestStoppedAndPendingServicesAreNotExpectedToExist(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", func(s *state.State) {
		s.Services["api"] = state.ServiceState{Status: domain.ServiceStopped, Checkout: state.Checkout{Path: "/gone/api"}, ContainerID: "old"}
		s.Services["web"] = state.ServiceState{Status: domain.ServicePending}
	})
	if res := f.run(false); len(res.Drift) != 0 {
		t.Errorf("a stopped ticket drifts: %s", kinds(res))
	}
}

func TestOrphansAreFoundByLabelAndFixRemovesOnlyThem(t *testing.T) {
	f := newFixture(t)
	live := f.runningTicket("demo")
	mk := func(name string, labels map[string]string) docker.ContainerInfo {
		c, err := f.dk.CreateContainer(ctx, docker.ContainerSpec{Name: name, Image: "img", Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		f.dk.StartContainer(ctx, c.ID)
		return c
	}
	ghost := mk("rig-ghost-api", docker.Labels("ghost", "api", docker.RoleService)) // no such ticket
	stranger := mk("rig-demo-other", docker.Labels("demo", "other", docker.RoleService))
	strangerDep := mk("rig-demo-cache", docker.Labels("demo", "cache", docker.RoleDependency))
	foreign := mk("somebody-elses", map[string]string{"app": "nginx"})      // not rig's
	almost := mk("rig-lookalike", map[string]string{"rig.role": "service"}) // has a rig label but no ticket label: not rig's to judge
	ghostNet, _ := f.dk.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-ghost", Labels: docker.Labels("ghost", "", docker.RoleNetwork)})
	foreignNet, _ := f.dk.CreateNetwork(ctx, docker.NetworkSpec{Name: "bridge-ish"})
	f.reconcileEvents()

	res := f.run(false)
	var orphans []string
	for _, d := range res.Drift {
		if !d.Fixable {
			t.Errorf("unexpected drift %+v", d)
		}
		orphans = append(orphans, d.Kind+":"+d.Detail)
	}
	joined := strings.Join(orphans, "\n")
	for _, want := range []string{
		`orphan_container:container rig-ghost-api: no ticket "ghost" exists`,
		`orphan_container:container rig-demo-other: ticket "demo" has no service "other"`,
		`orphan_container:container rig-demo-cache: ticket "demo" has no dependency "cache"`,
		`orphan_network:network rig-ghost: no ticket "ghost" exists`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	for _, name := range []string{"rig-demo-api", "somebody-elses", "rig-lookalike", "bridge-ish", "rig-demo-db"} {
		if strings.Contains(joined, name) {
			t.Errorf("%s was called an orphan", name)
		}
	}
	if len(f.reconcileEvents()) != 1 {
		t.Error("finding drift did not publish reconcile.done once")
	}

	// a plain check removes nothing
	if _, err := f.dk.InspectContainer(ctx, ghost.ID); err != nil {
		t.Fatal("the check removed an orphan")
	}

	fixed := f.run(true)
	if len(fixed.Failures) != 0 || len(fixed.Drift) != 0 {
		t.Fatalf("fix = %+v", fixed)
	}
	if strings.Join(fixed.Fixed, ",") != "container rig-ghost-api,container rig-demo-other,container rig-demo-cache,network rig-ghost" &&
		len(fixed.Fixed) != 4 {
		t.Errorf("fixed = %v", fixed.Fixed)
	}
	for _, id := range []string{ghost.ID, stranger.ID, strangerDep.ID} {
		if _, err := f.dk.InspectContainer(ctx, id); !errors.Is(err, docker.ErrNotFound) {
			t.Errorf("orphan %s survived the fix: %v", id, err)
		}
	}
	if _, err := f.dk.InspectNetwork(ctx, ghostNet.ID); !errors.Is(err, docker.ErrNotFound) {
		t.Error("the orphan network survived")
	}
	for _, c := range []docker.ContainerInfo{live.svc, live.dep, live.agent, foreign, almost} {
		if got, err := f.dk.InspectContainer(ctx, c.ID); err != nil || got.State != docker.StateRunning {
			t.Errorf("%s was touched by the fix: %v %v", c.Name, got.State, err)
		}
	}
	for _, id := range []string{live.network.ID, foreignNet.ID} {
		if _, err := f.dk.InspectNetwork(ctx, id); err != nil {
			t.Errorf("network %s was touched by the fix: %v", id, err)
		}
	}
	if again := f.run(false); len(again.Drift) != 0 {
		t.Errorf("drift after the fix: %s", kinds(again))
	}
}

func TestAStoppedTicketsLeftoversAreOrphans(t *testing.T) {
	f := newFixture(t)
	f.ticket("demo", nil) // never started: nothing recorded
	c, _ := f.dk.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-demo-api", Image: "img", Labels: docker.Labels("demo", "api", docker.RoleService)})
	f.dk.StartContainer(ctx, c.ID)
	n, _ := f.dk.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-demo", Labels: docker.Labels("demo", "", docker.RoleNetwork)})
	res := f.run(false)
	if len(res.Drift) != 2 || !strings.Contains(res.Drift[0].Detail+res.Drift[1].Detail, `ticket "demo" is stopped`) {
		t.Fatalf("drift = %+v", res.Drift)
	}
	if fixed := f.run(true); len(fixed.Fixed) != 2 || len(fixed.Failures) != 0 {
		t.Errorf("fix = %+v", fixed)
	}
	if _, err := f.dk.InspectContainer(ctx, c.ID); !errors.Is(err, docker.ErrNotFound) {
		t.Error("the leftover container survived")
	}
	if _, err := f.dk.InspectNetwork(ctx, n.ID); !errors.Is(err, docker.ErrNotFound) {
		t.Error("the leftover network survived")
	}
}

func TestATicketWithAnOperationUnderWayIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	f.dk.RemoveContainer(ctx, r.svc.ID, true) // would be drift...
	// ... and a container a start has just created but not yet recorded
	c, _ := f.dk.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-fresh-api", Image: "img", Labels: docker.Labels("fresh", "api", docker.RoleService)})
	f.dk.StartContainer(ctx, c.ID)
	f.ticket("fresh", nil)
	f.busy["demo"], f.busy["fresh"] = true, true
	if res := f.run(true); len(res.Drift) != 0 || len(res.Fixed) != 0 {
		t.Errorf("busy tickets were examined: %+v", res)
	}
	if _, err := f.dk.InspectContainer(ctx, c.ID); err != nil {
		t.Error("a container a start was creating was removed")
	}
	f.busy["demo"], f.busy["fresh"] = false, false
	if res := f.run(false); len(res.Drift) != 2 {
		t.Errorf("once idle: %s", kinds(res))
	}
}

func TestUnreadableTicketsAreNeverTreatedAsOrphanOwners(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.dir, "broken")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "ticket.yaml"), []byte("id: ["), 0o600)
	c, _ := f.dk.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-broken-api", Image: "img", Labels: docker.Labels("broken", "api", docker.RoleService)})
	f.dk.StartContainer(ctx, c.ID)
	if res := f.run(true); len(res.Drift) != 0 || len(res.Fixed) != 0 {
		t.Errorf("a ticket whose files cannot be read had its container judged: %+v", res)
	}
	if _, err := f.dk.InspectContainer(ctx, c.ID); err != nil {
		t.Error("its container was removed")
	}
}

func TestWhatCannotBeCheckedIsSaidNotAssumed(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	f.dk.Down(docker.ErrUnavailable)
	f.term.Vanish(r.pane)
	res := f.run(true)
	if kinds(res) != "demo/api:agent_pane_missing" {
		t.Errorf("with Docker down only panes are checked: %s", kinds(res))
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "Docker could not be checked") {
		t.Errorf("notes = %v", res.Notes)
	}
	f.dk.Up()
	f.term.Down(errors.New("herdr is not running"))
	res = f.run(false)
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "terminal backend could not be checked") || strings.Contains(kinds(res), "pane") {
		t.Errorf("with the terminal down: %+v", res)
	}
	// no Docker or backend configured at all: nothing is checked and nothing is claimed
	bare := f.build(nil, nil)
	if res, _ := bare.Reconcile(ctx, true); len(res.Notes) != 0 || strings.Contains(kinds(res), "container") {
		t.Errorf("bare = %+v", res)
	}
}

func TestEventsAreOnlyPublishedWhenTheFindingsChange(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	f.reconcileEvents()
	f.run(false)
	if len(f.reconcileEvents()) != 0 {
		t.Error("a clean pass announced something")
	}
	f.dk.RemoveContainer(ctx, r.svc.ID, true)
	f.run(false)
	if evs := f.reconcileEvents(); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"drift":1`) {
		t.Errorf("events = %+v", evs)
	}
	f.run(false)
	if len(f.reconcileEvents()) != 0 {
		t.Error("an unchanged finding was announced again")
	}
	f.dk.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-ghost-api", Image: "img", Labels: docker.Labels("ghost", "api", docker.RoleService)})
	f.run(false)
	if len(f.reconcileEvents()) != 1 {
		t.Error("a changed finding was not announced")
	}
}

func TestTheBackgroundLoopChecksAtOnceAndEveryInterval(t *testing.T) {
	f := newFixture(t)
	r := f.runningTicket("demo")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.r.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			f.clock.Advance(time.Minute)
			if ok() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	f.dk.RemoveContainer(ctx, r.svc.ID, true)
	waitFor("the loop to notice the killed container", func() bool { return len(f.r.Drift("demo")) == 1 })
	f.dk.CreateContainer(ctx, docker.ContainerSpec{Name: "x", Image: "img"})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop")
	}
}
