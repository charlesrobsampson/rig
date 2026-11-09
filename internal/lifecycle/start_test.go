package lifecycle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

var (
	ctx   = context.Background()
	epoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
)

type fixture struct {
	t       *testing.T
	dir     string
	states  *state.Manager
	bus     *events.Bus
	sub     *events.Subscription
	lc      *lifecycle.Manager
	creator *registry.Creator
}

func newFixture(t *testing.T, defaultSandbox domain.Sandbox) *fixture {
	t.Helper()
	dir := filepath.Join(testkit.RealTempDir(t), "tickets")
	clock := testkit.NewFakeClock(epoch)
	states := state.NewManager(clock)
	bus := events.NewBus(clock, 0)
	sub := bus.Subscribe()
	<-sub.Events() // the ack
	t.Cleanup(sub.Close)
	global := config.Global{Defaults: config.Defaults{Sandbox: defaultSandbox, BranchPrefix: "rig/"}}
	return &fixture{
		t: t, dir: dir, states: states, bus: bus, sub: sub,
		lc: lifecycle.New(lifecycle.Options{Dir: dir, States: states, Global: global, Bus: bus}),
		creator: registry.NewCreator(registry.CreatorOptions{
			Dir: dir, States: states, Sources: ticket.DefaultRegistry(),
			RepoCheck: func(p string) error { return git.CheckRepo(ctx, p) }, BranchPrefix: "rig/",
		}),
	}
}

// create makes a ticket with one service per repo, named svc1, svc2, ...
func (f *fixture) create(id string, repos ...*testkit.Repo) string {
	f.t.Helper()
	var services []api.ServiceSpec
	for i, r := range repos {
		services = append(services, api.ServiceSpec{Name: "svc" + string(rune('1'+i)), Repo: r.Dir})
	}
	res, err := f.creator.Create(ctx, api.TicketNewParams{ID: id, Title: "T " + id, Services: services})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.Path
}

func (f *fixture) start(id string, sandbox domain.Sandbox) (api.TicketStartResult, error) {
	return f.lc.Start(ctx, api.TicketStartParams{ID: id, Sandbox: sandbox})
}

func (f *fixture) mustStart(id string, sandbox domain.Sandbox) api.TicketStartResult {
	f.t.Helper()
	res, err := f.start(id, sandbox)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fixture) events() []api.Event {
	var out []api.Event
	for {
		select {
		case ev := <-f.sub.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (f *fixture) state(id string) state.State {
	f.t.Helper()
	s, err := f.states.Load(filepath.Join(f.dir, id, "state.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func apiCode(t *testing.T, err error) api.ErrorCode {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want an *api.Error", err)
	}
	return e.Code
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return testkit.RunGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func TestStartProvisionsOneCheckoutPerServiceChosenByTheSandbox(t *testing.T) {
	tests := []struct {
		name     string
		global   domain.Sandbox
		flag     domain.Sandbox
		want     domain.CheckoutKind
		dotGitIs string // "dir" for a clone's .git, "file" for a worktree's pointer
	}{
		{"container sandbox gives isolated clones", domain.SandboxContainer, "", domain.CheckoutClone, "dir"},
		{"host sandbox gives worktrees", domain.SandboxHost, "", domain.CheckoutWorktree, "file"},
		{"a flag overrides the configured sandbox", domain.SandboxContainer, domain.SandboxHost, domain.CheckoutWorktree, "file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.global)
			a, b := testkit.NewRepo(t), testkit.NewRepo(t)
			ticketDir := f.create("demo", a, b)

			res := f.mustStart("demo", tt.flag)
			if len(res.Checkouts) != 2 {
				t.Fatalf("checkouts = %+v", res.Checkouts)
			}
			for i, svc := range []string{"svc1", "svc2"} {
				co := res.Checkouts[i]
				wantPath := filepath.Join(ticketDir, svc)
				if co.Service != svc || co.Kind != tt.want || co.Path != wantPath || co.Branch != "rig/demo/"+svc || !co.Created {
					t.Errorf("%s checkout = %+v", svc, co)
				}
				if got := currentBranch(t, wantPath); got != "rig/demo/"+svc {
					t.Errorf("%s is on branch %q", svc, got)
				}
				info, err := os.Stat(filepath.Join(wantPath, ".git"))
				if err != nil || info.IsDir() != (tt.dotGitIs == "dir") {
					t.Errorf("%s: .git is wrong for a %s checkout (%v)", svc, tt.want, err)
				}
			}

			st := f.state("demo")
			for _, svc := range []string{"svc1", "svc2"} {
				got := st.Services[svc]
				if got.Status != domain.ServiceProvisioned || got.Checkout.Kind != tt.want || got.Checkout.Path != filepath.Join(ticketDir, svc) {
					t.Errorf("state for %s = %+v", svc, got)
				}
			}
			if res.Summary.Services[0].Status != domain.ServiceProvisioned {
				t.Errorf("summary = %+v", res.Summary)
			}
		})
	}
}

func TestStartHonorsAPerServiceSandbox(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	a, b := testkit.NewRepo(t), testkit.NewRepo(t)
	ticketDir := f.create("mixed", a, b)
	yaml := "id: mixed\ntitle: Mixed\nservices:\n" +
		"  - {name: svc1, repo: " + a.Dir + ", branch: rig/mixed/svc1}\n" +
		"  - {name: svc2, repo: " + b.Dir + ", branch: rig/mixed/svc2, sandbox: host}\n"
	if err := os.WriteFile(filepath.Join(ticketDir, "ticket.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	res := f.mustStart("mixed", "")
	if res.Checkouts[0].Kind != domain.CheckoutClone || res.Checkouts[1].Kind != domain.CheckoutWorktree {
		t.Errorf("kinds = %s, %s; want a clone for the container service and a worktree for the host one", res.Checkouts[0].Kind, res.Checkouts[1].Kind)
	}
}

func TestStartIsIdempotent(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	first := f.mustStart("demo", "")
	f.events() // clear

	work := filepath.Join(first.Checkouts[0].Path, "agent-work.txt")
	if err := os.WriteFile(work, []byte("in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := f.state("demo")

	second := f.mustStart("demo", "")
	if second.Checkouts[0].Created {
		t.Error("the second start reported creating a checkout that already existed")
	}
	if got, _ := os.ReadFile(work); string(got) != "in progress" {
		t.Error("restarting disturbed the working tree")
	}
	if after := f.state("demo"); !reflect.DeepEqual(after.Services["svc1"], before.Services["svc1"]) {
		t.Errorf("state changed on a no-op start: %+v vs %+v", after.Services["svc1"], before.Services["svc1"])
	}
	if evs := f.events(); len(evs) != 0 {
		t.Errorf("a no-op start published %d event(s)", len(evs))
	}
}

func TestStartAnnouncesWhatItDid(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	f.mustStart("demo", "")

	var types []string
	for _, ev := range f.events() {
		types = append(types, string(ev.Type)+"/"+ev.Service)
	}
	want := []string{"service.status/svc1", "service.status/svc2", "ticket.started/"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", types, want)
	}
}

func TestAFailureOnALaterServiceRollsBackTheEarlierOnes(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	a, b := testkit.NewRepo(t), testkit.NewRepo(t)
	ticketDir := f.create("demo", a, b)
	yaml := "id: demo\ntitle: T\nservices:\n" +
		"  - {name: svc1, repo: " + a.Dir + ", branch: rig/demo/svc1}\n" +
		"  - {name: svc2, repo: " + b.Dir + ", branch: rig/demo/svc2, base: no-such-base}\n"
	os.WriteFile(filepath.Join(ticketDir, "ticket.yaml"), []byte(yaml), 0o600)
	before := f.state("demo")

	_, err := f.start("demo", "")
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeInvalid || !strings.Contains(e.Message, "svc2") || !strings.Contains(e.Message, "no-such-base") {
		t.Fatalf("error = %v, want invalid naming svc2 and the missing base", err)
	}
	if exists(filepath.Join(ticketDir, "svc1")) || exists(filepath.Join(ticketDir, "svc2")) {
		t.Error("a failed start left checkouts behind")
	}
	if after := f.state("demo"); !reflect.DeepEqual(after.Services["svc1"], before.Services["svc1"]) || after.Services["svc1"].Checkout.Path != "" {
		t.Errorf("state was changed by a failed start: %+v", after.Services["svc1"])
	}
	if evs := f.events(); len(evs) != 0 {
		t.Errorf("a failed start published %d event(s)", len(evs))
	}

	// Fixing the problem and starting again works.
	os.WriteFile(filepath.Join(ticketDir, "ticket.yaml"), []byte(strings.Replace(yaml, ", base: no-such-base", "", 1)), 0o600)
	if res, err := f.start("demo", ""); err != nil || len(res.Checkouts) != 2 {
		t.Errorf("start after the fix = %+v, %v", res, err)
	}
}

func TestStartAdoptsACheckoutALostStateRecordLeftBehind(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	ticketDir := f.create("demo", repo)
	// A crash after provisioning but before the state was written.
	if _, err := git.Provision(ctx, git.Spec{Repo: repo.Dir, Branch: "rig/demo/svc1", Path: filepath.Join(ticketDir, "svc1"), Kind: domain.CheckoutClone}); err != nil {
		t.Fatal(err)
	}

	res := f.mustStart("demo", "")
	if res.Checkouts[0].Created {
		t.Error("an existing valid checkout should be adopted, not reported as created")
	}
	if got := f.state("demo").Services["svc1"]; got.Checkout.Path != filepath.Join(ticketDir, "svc1") || got.Status != domain.ServiceProvisioned {
		t.Errorf("state = %+v; the adopted checkout should now be recorded", got)
	}
}

func TestStartReprovisionsACheckoutThatWasDeleted(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t))
	first := f.mustStart("demo", "")
	os.RemoveAll(first.Checkouts[0].Path)

	res := f.mustStart("demo", "")
	if !res.Checkouts[0].Created || !exists(filepath.Join(res.Checkouts[0].Path, ".git")) {
		t.Errorf("checkout = %+v; a deleted checkout should be provisioned again", res.Checkouts[0])
	}
}

func TestStartRefusesAPathThatIsNotAMatchingCheckout(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	a, b := testkit.NewRepo(t), testkit.NewRepo(t)
	ticketDir := f.create("demo", a, b)
	os.MkdirAll(filepath.Join(ticketDir, "svc2"), 0o755)
	os.WriteFile(filepath.Join(ticketDir, "svc2", "mine.txt"), []byte("not a checkout"), 0o644)

	_, err := f.start("demo", "")
	if apiCode(t, err) != api.CodeConflict || !strings.Contains(err.Error(), "svc2") {
		t.Fatalf("error = %v, want a conflict naming svc2", err)
	}
	if exists(filepath.Join(ticketDir, "svc1")) {
		t.Error("svc1's checkout was not rolled back")
	}
	if got, _ := os.ReadFile(filepath.Join(ticketDir, "svc2", "mine.txt")); string(got) != "not a checkout" {
		t.Error("the user's files were touched")
	}

	t.Run("a worktree where a clone is expected is also refused", func(t *testing.T) {
		g := newFixture(t, domain.SandboxHost)
		repo := testkit.NewRepo(t)
		dir := g.create("demo", repo)
		g.mustStart("demo", "")
		_ = dir
		if _, err := g.start("demo", domain.SandboxContainer); apiCode(t, err) != api.CodeConflict {
			t.Errorf("error = %v, want a conflict: the existing worktree is not a clone", err)
		}
	})
}

func TestStartReportsABranchCheckedOutInTheMainRepoAsAConflict(t *testing.T) {
	f := newFixture(t, domain.SandboxHost)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	repo.Git("checkout", "-q", "-b", "rig/demo/svc1")

	_, err := f.start("demo", "")
	if apiCode(t, err) != api.CodeConflict || !strings.Contains(err.Error(), "already checked out") {
		t.Errorf("error = %v, want a conflict", err)
	}
}

func TestStartErrors(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	ticketDir := f.create("demo", repo)

	tests := []struct {
		name  string
		id    string
		setup func()
		code  api.ErrorCode
		text  string
	}{
		{"an unknown ticket", "nope", nil, api.CodeNotFound, "nope"},
		{"an invalid id", "../etc", nil, api.CodeInvalid, "ticket id"},
		{"a broken ticket.yaml", "demo", func() { os.WriteFile(filepath.Join(ticketDir, "ticket.yaml"), []byte("id: demo\nservices: ["), 0o600) }, api.CodeInvalid, "ticket.yaml"},
		{"a missing state.json", "demo", func() {
			os.WriteFile(filepath.Join(ticketDir, "ticket.yaml"), []byte("id: demo\ntitle: T\nservices:\n  - {name: svc1, repo: "+repo.Dir+", branch: b}\n"), 0o600)
			os.Remove(filepath.Join(ticketDir, "state.json"))
		}, api.CodeInvalid, "state.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup()
			}
			_, err := f.start(tt.id, "")
			var e *api.Error
			if !errors.As(err, &e) || e.Code != tt.code || !strings.Contains(e.Message, tt.text) {
				t.Errorf("error = %v, want %s mentioning %q", err, tt.code, tt.text)
			}
		})
	}

	t.Run("a repository that no longer exists", func(t *testing.T) {
		g := newFixture(t, domain.SandboxContainer)
		r := testkit.NewRepo(t)
		g.create("gone", r)
		os.RemoveAll(r.Dir)
		_, err := g.start("gone", "")
		var e *api.Error
		if !errors.As(err, &e) || e.Code != api.CodeInvalid || !strings.Contains(e.Message, "svc1") {
			t.Errorf("error = %v, want invalid naming the service", err)
		}
	})

	t.Run("a finished ticket", func(t *testing.T) {
		g := newFixture(t, domain.SandboxContainer)
		dir := g.create("done", testkit.NewRepo(t))
		if err := g.states.Update(filepath.Join(dir, "state.json"), func(s *state.State) error { s.Phase = domain.PhaseDone; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := g.start("done", ""); apiCode(t, err) != api.CodeConflict {
			t.Errorf("error = %v, want a conflict", err)
		}
	})
}

func TestConcurrentStartsCreateEachCheckoutOnce(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	var wg sync.WaitGroup
	created := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.start("demo", "")
			if err != nil {
				t.Error(err)
				return
			}
			n := 0
			for _, c := range res.Checkouts {
				if c.Created {
					n++
				}
			}
			created <- n
		}()
	}
	wg.Wait()
	close(created)
	total := 0
	for n := range created {
		total += n
	}
	if total != 2 {
		t.Errorf("%d checkouts were created across concurrent starts, want exactly 2 (one per service)", total)
	}
}

func TestAStartedTicketShowsUpAsProvisioned(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", "")
	list, err := registry.New(f.dir, f.states).List()
	if err != nil || len(list) != 1 || list[0].Services[0].Status != domain.ServiceProvisioned || list[0].Error != "" {
		t.Errorf("list = %+v, %v", list, err)
	}
}
