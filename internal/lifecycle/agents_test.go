package lifecycle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// agentFixture is a fixture whose lifecycle also runs agents, in a fake terminal.
func agentFixture(t *testing.T, mutate func(*lifecycle.Options)) (*fixture, *testkit.FakeTerminal) {
	t.Helper()
	f := newFixture(t, domain.SandboxContainer)
	term := testkit.NewFakeTerminal()
	opts := lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch),
		Global:  config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, BranchPrefix: "rig/"}},
		Backend: term, BackendName: "fake", RigBinary: "/opt/rig bin/rig", AgentEnv: map[string]string{"RIG_HOME": "/rig/home"},
	}
	if mutate != nil {
		mutate(&opts)
	}
	f.lc = lifecycle.New(opts)
	return f, term
}

func agentByService(res api.TicketStartResult) map[string]api.AgentInfo {
	out := map[string]api.AgentInfo{}
	for _, a := range res.Agents {
		out[a.Service] = a
	}
	return out
}

func TestStartLaunchesOneAgentPerHostService(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxHost)

	spawned := term.Spawned()
	if len(spawned) != 2 {
		t.Fatalf("spawned %d panes, want 2", len(spawned))
	}
	for i, sp := range spawned {
		svc := "svc" + string(rune('1'+i))
		wantDir := res.Checkouts[i].Path
		if sp.Ticket != "demo" || sp.Service != svc || sp.Dir != wantDir {
			t.Errorf("pane %d = %s/%s in %s, want demo/%s in %s", i, sp.Ticket, sp.Service, sp.Dir, svc, wantDir)
		}
		if sp.Command[0] != "claude" || !strings.Contains(sp.Command[len(sp.Command)-1], `service "`+svc+`"`) {
			t.Errorf("command = %.60q", sp.Command)
		}
		if sp.Env["RIG_TICKET"] != "demo" || sp.Env["RIG_SERVICE"] != svc || sp.Env["RIG_HOME"] != "/rig/home" {
			t.Errorf("env = %v", sp.Env)
		}
		if !strings.Contains(sp.Command[2], `'/opt/rig bin/rig' agent-event idle`) {
			t.Errorf("hooks should run the daemon's own rig binary, quoted: %s", sp.Command[2])
		}
	}
	agents := agentByService(res)
	st := f.state("demo")
	for _, svc := range []string{"svc1", "svc2"} {
		a := agents[svc]
		if !a.Launched || a.PaneID == "" || a.Error != "" {
			t.Errorf("agent %s = %+v", svc, a)
		}
		rec := st.Services[svc].Agent
		if rec.PaneID != a.PaneID || rec.Backend != "fake" || rec.State != domain.AgentUnknown {
			t.Errorf("recorded agent %s = %+v", svc, rec)
		}
	}
}

func TestContainerSandboxServicesGetNoHostAgent(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	if len(term.Spawned()) != 0 {
		t.Error("an agent for a container-sandbox service was started on the host")
	}
	a := agentByService(res)["svc1"]
	if a.Launched || a.Error != "" || !strings.Contains(a.Note, "container") {
		t.Errorf("agent = %+v, want a note explaining why", a)
	}
}

func TestStartingAgainKeepsRunningAgentsAndRelaunchesFinishedOnes(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	first := f.mustStart("demo", domain.SandboxHost)
	pane := agentByService(first)["svc1"].PaneID

	again := f.mustStart("demo", domain.SandboxHost)
	if a := agentByService(again)["svc1"]; a.Launched || a.PaneID != pane || a.Note != "already running" {
		t.Errorf("second start: %+v", a)
	}
	if len(term.Spawned()) != 1 {
		t.Errorf("spawned %d panes, want 1", len(term.Spawned()))
	}

	term.Exit(terminal.PaneID(pane), 1) // the pane stays listed, like tmux
	third := f.mustStart("demo", domain.SandboxHost)
	a := agentByService(third)["svc1"]
	if !a.Launched || a.PaneID == pane || a.Error != "" {
		t.Errorf("after the agent exited: %+v", a)
	}
	if got := f.state("demo").Services["svc1"].Agent.PaneID; got != a.PaneID {
		t.Errorf("recorded pane = %s, want %s", got, a.PaneID)
	}
}

func TestAnAgentThatCannotStartIsReportedAndTheCheckoutsStay(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	term.Down(terminal.ErrUnavailable)
	res := f.mustStart("demo", domain.SandboxHost)
	a := agentByService(res)["svc1"]
	if a.Launched || a.Error == "" {
		t.Fatalf("agent = %+v, want an error", a)
	}
	if !exists(res.Checkouts[0].Path) || f.state("demo").Services["svc1"].Status != domain.ServiceProvisioned {
		t.Error("the checkout should be provisioned even though the agent could not start")
	}
	if f.state("demo").Services["svc1"].Agent.PaneID != "" {
		t.Error("a failed launch recorded a pane")
	}

	term.Up()
	if a := agentByService(f.mustStart("demo", domain.SandboxHost))["svc1"]; !a.Launched {
		t.Errorf("a second start should retry: %+v", a)
	}
}

func TestAgentSecrets(t *testing.T) {
	withProfile := func(o *lifecycle.Options) {
		o.Global.Auth = config.Auth{Profiles: map[string]config.AuthProfile{"work": {Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}}}
	}
	setAuth := func(t *testing.T, f *fixture, path string) {
		raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
		os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("auth: work\n")...), 0o600)
	}

	t.Run("without a resolver the launch says why", func(t *testing.T) {
		f, term := agentFixture(t, withProfile)
		setAuth(t, f, f.create("demo", testkit.NewRepo(t)))
		a := agentByService(f.mustStart("demo", domain.SandboxHost))["svc1"]
		if a.Launched || !strings.Contains(a.Error, "WORK_TOKEN") || len(term.Spawned()) != 0 {
			t.Errorf("agent = %+v", a)
		}
	})
	t.Run("a resolver supplies the value to the environment only", func(t *testing.T) {
		var asked []string
		f, term := agentFixture(t, func(o *lifecycle.Options) {
			withProfile(o)
			o.Secrets = func(names []string) (map[string]string, error) {
				asked = names
				return map[string]string{"WORK_TOKEN": "tok-sentinel"}, nil
			}
		})
		setAuth(t, f, f.create("demo", testkit.NewRepo(t)))
		res := f.mustStart("demo", domain.SandboxHost)
		sp := term.Spawned()[0]
		if sp.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "tok-sentinel" || len(asked) != 1 || asked[0] != "WORK_TOKEN" {
			t.Errorf("env = %v, asked = %v", sp.Env, asked)
		}
		for _, arg := range sp.Command {
			if strings.Contains(arg, "tok-sentinel") {
				t.Error("the secret value is in the command line")
			}
		}
		raw, _ := os.ReadFile(filepath.Join(f.dir, "demo", "state.json"))
		if strings.Contains(string(raw), "tok-sentinel") || strings.Contains(res.Agents[0].Error, "tok-sentinel") {
			t.Error("the secret value leaked into state or the result")
		}
	})
	t.Run("a resolver failure refuses the start before anything is created", func(t *testing.T) {
		f, _ := agentFixture(t, func(o *lifecycle.Options) {
			withProfile(o)
			o.Secrets = func([]string) (map[string]string, error) { return nil, errors.New("keychain locked") }
		})
		setAuth(t, f, f.create("demo", testkit.NewRepo(t)))
		_, err := f.start("demo", domain.SandboxHost)
		if apiCode(t, err) != api.CodeInvalid || !strings.Contains(err.Error(), "keychain locked") {
			t.Errorf("error = %v", err)
		}
		if exists(filepath.Join(f.dir, "demo", "svc1")) {
			t.Error("a checkout was created before the secrets were checked")
		}
	})
}

// closeOrder records whether the checkout still existed when the pane was closed.
type closeOrder struct {
	*testkit.FakeTerminal
	checkout       string
	existedAtClose bool
}

func (c *closeOrder) Close(ctx context.Context, id terminal.PaneID) error {
	c.existedAtClose = exists(c.checkout)
	return c.FakeTerminal.Close(ctx, id)
}

func TestStopClosesAgentsBeforeRemovingCheckouts(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxHost)
	order := &closeOrder{FakeTerminal: term, checkout: res.Checkouts[0].Path}
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch), Backend: order,
		Global: config.Global{Defaults: config.Defaults{Sandbox: domain.SandboxContainer, BranchPrefix: "rig/"}},
	})
	f.events()

	f.mustStop("demo", false, false)
	if !order.existedAtClose {
		t.Error("the checkout was removed before the agent's pane was closed")
	}
	if panes, _ := term.List(ctx); len(panes) != 0 {
		t.Errorf("panes left: %v", panes)
	}
	agent := f.state("demo").Services["svc1"].Agent
	if agent.PaneID != "" || agent.State != domain.AgentExited {
		t.Errorf("recorded agent = %+v", agent)
	}
	var announced bool
	for _, ev := range f.events() {
		announced = announced || ev.Type == api.EventAgentState
	}
	if !announced {
		t.Error("no agent.state event for the closed agent")
	}
}

func TestStopWhenThePaneIsAlreadyGone(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxHost)
	term.Vanish(terminal.PaneID(res.Agents[0].PaneID))
	f.mustStop("demo", false, false)
	if f.state("demo").Services["svc1"].Agent.PaneID != "" {
		t.Error("the vanished pane is still recorded")
	}
}

func TestStopWithAnUnreachableBackendKeepsTheCheckoutUnlessForced(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxHost)
	term.Down(terminal.ErrUnavailable)

	_, err := f.stop("demo", false, false)
	if apiCode(t, err) != api.CodeUnavailable || !exists(res.Checkouts[0].Path) {
		t.Fatalf("error = %v; the checkout must stay while the agent cannot be closed", err)
	}
	if _, err := f.stop("demo", true, false); err != nil {
		t.Errorf("a forced stop should carry on: %v", err)
	}
	if exists(res.Checkouts[0].Path) {
		t.Error("the forced stop left the checkout")
	}
}

func TestAttach(t *testing.T) {
	f, term := agentFixture(t, nil)
	f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxHost)
	f.create("solo", testkit.NewRepo(t))
	f.mustStart("solo", domain.SandboxContainer)

	pane2 := res.Agents[1].PaneID
	got, err := f.lc.Attach(ctx, api.AgentAttachParams{Ticket: "demo", Service: "svc2"})
	want, _ := term.Attach(ctx, terminal.PaneID(pane2))
	if err != nil || got.Service != "svc2" || strings.Join(got.Command, " ") != strings.Join(want, " ") {
		t.Errorf("Attach = %+v, %v; want the command for pane %s (%v)", got, err, pane2, want)
	}

	tests := []struct {
		name string
		p    api.AgentAttachParams
		code api.ErrorCode
		msg  string
	}{
		{"an unknown service", api.AgentAttachParams{Ticket: "demo", Service: "nope"}, api.CodeNotFound, "nope"},
		{"an unknown ticket", api.AgentAttachParams{Ticket: "ghost"}, api.CodeNotFound, "ghost"},
		{"several services and none named", api.AgentAttachParams{Ticket: "demo"}, api.CodeInvalid, "svc1, svc2"},
		{"a service with no agent", api.AgentAttachParams{Ticket: "solo"}, api.CodeNotRunning, "start the ticket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.lc.Attach(ctx, tt.p)
			if apiCode(t, err) != tt.code || !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("error = %v, want %s mentioning %q", err, tt.code, tt.msg)
			}
		})
	}

	t.Run("the only service need not be named", func(t *testing.T) {
		f.create("one", testkit.NewRepo(t))
		f.mustStart("one", domain.SandboxHost)
		if got, err := f.lc.Attach(ctx, api.AgentAttachParams{Ticket: "one"}); err != nil || got.Service != "svc1" {
			t.Errorf("Attach = %+v, %v", got, err)
		}
	})
	t.Run("a pane that is gone", func(t *testing.T) {
		term.Vanish(terminal.PaneID(pane2))
		_, err := f.lc.Attach(ctx, api.AgentAttachParams{Ticket: "demo", Service: "svc2"})
		if apiCode(t, err) != api.CodeNotRunning {
			t.Errorf("error = %v", err)
		}
	})
}
