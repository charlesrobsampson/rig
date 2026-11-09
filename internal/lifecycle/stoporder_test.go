package lifecycle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// timeline records, in order, what a stop does to Docker and to the terminal, and
// whether the checkout still existed at that moment.
type timeline struct {
	mu       sync.Mutex
	events   []string
	checkout string
	failOn   map[string]error // container name -> error for RemoveContainer
}

func (tl *timeline) add(what string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	state := "checkout present"
	if tl.checkout != "" && !exists(tl.checkout) {
		state = "checkout gone"
	}
	tl.events = append(tl.events, what+" ("+state+")")
}

func (tl *timeline) log() []string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return append([]string(nil), tl.events...)
}

type recordingDocker struct {
	*testkit.FakeDocker
	tl *timeline
}

func (r *recordingDocker) RemoveContainer(ctx context.Context, name string, force bool) error {
	if err := r.tl.failOn[strings.TrimPrefix(name, "")]; err != nil {
		return err
	}
	// the fake accepts ids; resolve to the name for the log
	label := name
	if c, err := r.FakeDocker.InspectContainer(ctx, name); err == nil {
		label = c.Name
		if err := r.tl.failOn[c.Name]; err != nil {
			return err
		}
	}
	err := r.FakeDocker.RemoveContainer(ctx, name, force)
	if err == nil {
		r.tl.add("remove container " + label)
	}
	return err
}

func (r *recordingDocker) RemoveNetwork(ctx context.Context, name string) error {
	label := name
	if n, err := r.FakeDocker.InspectNetwork(ctx, name); err == nil {
		label = n.Name
	}
	err := r.FakeDocker.RemoveNetwork(ctx, name)
	if err == nil {
		r.tl.add("remove network " + label)
	}
	return err
}

type recordingTerminal struct {
	*testkit.FakeTerminal
	tl      *timeline
	onClose func()
}

func (r *recordingTerminal) Close(ctx context.Context, id terminal.PaneID) error {
	r.tl.add("close pane")
	if r.onClose != nil {
		r.onClose()
	}
	return r.FakeTerminal.Close(ctx, id)
}

// stopFixture is a started internal-mode ticket with a service, a dependency, an agent
// and the proxy, wired through recorders.
func stopFixture(t *testing.T) (*fixture, *recordingDocker, *recordingTerminal, *timeline, api.TicketStartResult) {
	t.Helper()
	tl := &timeline{failOn: map[string]error{}}
	var rd *recordingDocker
	var rt *recordingTerminal
	f, dk, term := agentsFixture(t, func(o *lifecycle.Options) {
		o.Global.Defaults.NetworkMode = domain.NetworkInternal
		rd = &recordingDocker{FakeDocker: nil, tl: tl}
		_ = rd
	})
	rd = &recordingDocker{FakeDocker: dk, tl: tl}
	rt = &recordingTerminal{FakeTerminal: term, tl: tl}
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, apiService)
	addDeps(t, path, dbDeps)
	res := f.mustStart("demo", domain.SandboxContainer)
	tl.checkout = res.Checkouts[0].Path

	// from here on the lifecycle talks to the recorders
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Docker: rd, Backend: rt, BackendName: "fake", EgressBinary: fakeProxyBinary,
		HealthProbe:         func(context.Context, string) bool { return true },
		Global:              globalWithMode(domain.NetworkInternal),
		ServiceStartTimeout: 300 * time.Millisecond, HealthPoll: time.Millisecond, Settle: 20 * time.Millisecond, StopGrace: time.Second,
	})
	return f, rd, rt, tl, res
}

func TestStopTearsDownInOrderAndRemovesTheCheckoutLast(t *testing.T) {
	f, _, _, tl, res := stopFixture(t)
	f.mustStop("demo", false, false)

	got := tl.log()
	var order []string
	for _, e := range got {
		order = append(order, e)
	}
	// agents (pane, then its container), the service, the dependency, the proxy, the networks
	want := []string{
		"close pane (checkout present)",
		"remove container rig-demo-svc1-agent (checkout present)",
		"remove container rig-demo-svc1 (checkout present)",
		"remove container rig-demo-dep-db (checkout present)",
		"remove container rig-demo-proxy (checkout present)",
		"remove network rig-demo-egress (checkout present)",
		"remove network rig-demo (checkout present)",
	}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Errorf("order of operations:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
	if exists(res.Checkouts[0].Path) {
		t.Error("the checkout remains")
	}
}

func TestADirtyCheckoutAbortsBeforeAnythingIsTouched(t *testing.T) {
	f, rd, rt, tl, res := stopFixture(t)
	os.WriteFile(filepath.Join(res.Checkouts[0].Path, "unsaved.txt"), []byte("work in progress"), 0o600)

	_, err := f.stop("demo", false, false)
	if apiCode(t, err) != api.CodeBlocked || !strings.Contains(err.Error(), "Nothing was stopped") {
		t.Fatalf("error = %v", err)
	}
	if ev := tl.log(); len(ev) != 0 {
		t.Errorf("a refused stop touched things: %v", ev)
	}
	for _, name := range []string{"rig-demo-svc1", "rig-demo-svc1-agent", "rig-demo-dep-db", "rig-demo-proxy"} {
		if c, err := rd.InspectContainer(ctx, name); err != nil || c.State != docker.StateRunning {
			t.Errorf("%s = %+v, %v; it must still be running", name, c, err)
		}
	}
	if panes, _ := rt.List(ctx); len(panes) != 1 {
		t.Errorf("the agent's pane was closed: %v", panes)
	}
	if _, err := rd.InspectNetwork(ctx, "rig-demo"); err != nil {
		t.Errorf("the network is gone: %v", err)
	}
	if !exists(filepath.Join(res.Checkouts[0].Path, "unsaved.txt")) {
		t.Error("the unsaved work was deleted")
	}
	if f.state("demo").Services["svc1"].HostPort == 0 {
		t.Error("the port was released by a refused stop")
	}

	// --force proceeds
	f.mustStop("demo", true, false)
	if list, _ := rd.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers left after a forced stop: %v", list)
	}
}

func TestAnAgentsLastCommitDuringShutdownIsStillImported(t *testing.T) {
	f, _, rt, _, res := stopFixture(t)
	// the agent commits while it is being closed, after the first look at the checkout
	rt.onClose = func() {
		path := res.Checkouts[0].Path
		os.WriteFile(filepath.Join(path, "final.txt"), []byte("final\n"), 0o600)
		testkit.RunGit(t, path, "add", "-A")
		testkit.RunGit(t, path, "-c", "user.name=agent", "-c", "user.email=a@b", "commit", "-qm", "final commit while closing")
	}
	f.mustStop("demo", false, false)
	repo := res.Checkouts[0]
	_ = repo
	out := testkit.RunGit(t, originOf(t, f), "log", "--oneline", "rig/demo/svc1")
	if !strings.Contains(out, "final commit while closing") {
		t.Errorf("the main repository's branch lacks the last commit:\n%s", out)
	}
}

func originOf(t *testing.T, f *fixture) string {
	t.Helper()
	// the first service's repository is the ticket's origin: read it from ticket.yaml
	raw, _ := os.ReadFile(filepath.Join(f.dir, "demo", "ticket.yaml"))
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "repo: "); ok && k == "" {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("no repo in ticket.yaml")
	return ""
}

func TestPortsAreFreedAndStateIsClean(t *testing.T) {
	f, rd, _, _, _ := stopFixture(t)
	before := f.state("demo").Services["svc1"].HostPort
	if before == 0 {
		t.Fatal("the service had no host port")
	}
	f.mustStop("demo", false, false)
	st := f.state("demo")
	if len(rd.HostPorts()) != 0 {
		t.Errorf("ports still held by containers: %v", rd.HostPorts())
	}
	if st.Services["svc1"].HostPort != 0 || st.Services["svc1"].ContainerID != "" || st.Network.Name != "" || st.EgressNetwork != nil {
		t.Errorf("state = %+v / %+v", st.Services["svc1"], st)
	}
	if d := st.Dependencies["db"]; d.ContainerID != "" || d.Status != domain.ServiceStopped {
		t.Errorf("dependency state = %+v", d)
	}
	if st.Services["svc1"].Agent.PaneID != "" || st.Services["svc1"].Agent.State != domain.AgentExited {
		t.Errorf("agent state = %+v", st.Services["svc1"].Agent)
	}
}

func TestAPartialFailureIsReportedWithWhatRemainsAndRetries(t *testing.T) {
	f, rd, _, tl, res := stopFixture(t)
	tl.failOn["rig-demo-svc1"] = errors.New("Error response from daemon: device or resource busy")

	_, err := f.stop("demo", false, false)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "did not finish") {
		t.Fatalf("error = %v", err)
	}
	details := strings.Join(apiErr.Details, "\n")
	for _, want := range []string{"service svc1", "device or resource busy", "networks were left in place", "still there:", "container rig-demo-svc1", "network rig-demo", "the checkouts were kept"} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q:\n%s", want, details)
		}
	}
	// what could be removed was removed
	for _, gone := range []string{"rig-demo-svc1-agent", "rig-demo-dep-db", "rig-demo-proxy"} {
		if _, err := rd.InspectContainer(ctx, gone); err == nil {
			t.Errorf("%s was not removed although it was independent of the failure", gone)
		}
	}
	if !exists(res.Checkouts[0].Path) {
		t.Error("the checkout was removed under a container that could not be stopped")
	}
	st := f.state("demo")
	if st.Dependencies["db"].ContainerID != "" {
		t.Errorf("state still lists the removed dependency: %+v", st.Dependencies["db"])
	}
	if st.Network.Name == "" {
		t.Error("the network is still there, so state must still name it")
	}

	// fix the problem and stop again: it finishes
	delete(tl.failOn, "rig-demo-svc1")
	f.mustStop("demo", false, false)
	if list, _ := rd.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers left: %v", list)
	}
	if nets, _ := rd.ListNetworks(ctx, nil); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
	if exists(res.Checkouts[0].Path) {
		t.Error("the checkout remains after the retry")
	}
}

func TestStoppingTwiceIsANoOp(t *testing.T) {
	f, rd, _, tl, _ := stopFixture(t)
	f.mustStop("demo", false, false)
	events := len(tl.log())
	f.events()

	res, err := f.stop("demo", false, false)
	if err != nil || len(res.Services) != 0 || len(res.Resources) != 0 {
		t.Errorf("second stop = %+v, %v; want a quiet success", res, err)
	}
	if len(tl.log()) != events {
		t.Errorf("the second stop touched things: %v", tl.log()[events:])
	}
	for _, ev := range f.events() {
		t.Errorf("the second stop published %s", ev.Type)
	}
	_ = rd
}

func TestStopSweepsLeftoversLabelledForTheTicket(t *testing.T) {
	f, rd, _, _, _ := stopFixture(t)
	// a container an earlier crash left behind, known to no state file
	c, _ := rd.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-demo-orphan", Image: "postgres:16", Labels: docker.Labels("demo", "ghost", docker.RoleService), Network: "rig-demo"})
	rd.StartContainer(ctx, c.ID)
	res := f.mustStop("demo", false, false)
	if list, _ := rd.ListContainers(ctx, map[string]string{"rig.ticket": "demo"}); len(list) != 0 {
		t.Errorf("leftovers: %v", list)
	}
	if !strings.Contains(strings.Join(res.Resources, ","), "container rig-demo-orphan") {
		t.Errorf("the result does not list the leftover it removed: %v", res.Resources)
	}
}

func TestStopListsEverythingItRemoved(t *testing.T) {
	f, _, _, _, _ := stopFixture(t)
	res := f.mustStop("demo", false, false)
	want := "container rig-demo-dep-db,container rig-demo-proxy,container rig-demo-svc1,container rig-demo-svc1-agent,network rig-demo,network rig-demo-egress"
	if got := strings.Join(res.Resources, ","); got != want {
		t.Errorf("resources = %s\nwant %s", got, want)
	}
}

func TestKeepCheckoutsStillTearsDownContainersAndNetworks(t *testing.T) {
	f, rd, _, _, res := stopFixture(t)
	f.mustStop("demo", false, true)
	if list, _ := rd.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers left: %v", list)
	}
	if !exists(res.Checkouts[0].Path) {
		t.Error("--keep-checkouts removed the checkout")
	}
}

func globalWithMode(mode domain.NetworkMode) config.Global {
	return config.Global{
		Defaults: config.Defaults{Sandbox: domain.SandboxContainer, NetworkMode: mode, BranchPrefix: "rig/"},
		Docker:   config.Docker{AgentBase: "debian:bookworm-slim", ClaudeVersion: "latest"},
		Egress:   config.Egress{DefaultAllow: []string{"api.anthropic.com"}, DefaultPorts: []int{443}},
	}
}
