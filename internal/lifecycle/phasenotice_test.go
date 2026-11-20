package lifecycle_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

type phaseNotices struct {
	ticket  string
	notices map[string]string
}

func TestEveryRunningAgentGetsANoticeWithTheNewPhasesRules(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	var got []phaseNotices
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch), Global: config.Global{Defaults: config.Defaults{BranchPrefix: "rig/"}},
		OnPhaseChanged: func(ticket string, notices map[string]string) { got = append(got, phaseNotices{ticket, notices}) },
	})
	dir := f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t), testkit.NewRepo(t))
	agree(t, dir)
	f.states.Update(filepath.Join(dir, "state.json"), func(s *state.State) error {
		s.Services["svc1"] = state.ServiceState{Agent: state.Agent{PaneID: "p1", ContainerID: "c1", State: domain.AgentIdle}}
		s.Services["svc2"] = state.ServiceState{Agent: state.Agent{PaneID: "p2", State: domain.AgentWorking}}
		s.Services["svc3"] = state.ServiceState{Agent: state.Agent{PaneID: "p3", State: domain.AgentExited}}
		return nil
	})

	f.advanceTo("demo", domain.PhaseImplement)
	if len(got) != 1 || got[0].ticket != "demo" || len(got[0].notices) != 2 {
		t.Fatalf("notices = %+v, want one call for svc1 and svc2 (svc3 has exited)", got)
	}
	in, host := got[0].notices["svc1"], got[0].notices["svc2"]
	for _, want := range []string{"from plan to implement", "(/rig/ticket)", "Commit each change", "## manual-tests.md"} {
		if !strings.Contains(in, want) {
			t.Errorf("container agent's notice lacks %q:\n%s", want, in)
		}
	}
	if !strings.Contains(host, "("+dir+")") {
		t.Errorf("host agent's notice does not name the ticket directory %s:\n%s", dir, host)
	}

	f.mustSet("demo", domain.PhasePlan, "rethink the API")
	if len(got) != 2 || !strings.Contains(got[1].notices["svc1"], "from implement back to plan. Reason: rethink the API.") {
		t.Errorf("backward move notices = %+v", got[1:])
	}
}

func TestAFailedMoveTellsNoAgent(t *testing.T) {
	f := newFixtureWithGates(t, domain.SandboxContainer, strict)
	called := false
	f.lc = lifecycle.New(lifecycle.Options{
		Dir: f.dir, States: f.states, Bus: f.bus, Clock: testkit.NewFakeClock(epoch), Global: config.Global{Defaults: config.Defaults{BranchPrefix: "rig/", Policy: domain.Policy{Gates: strict}}},
		OnPhaseChanged: func(string, map[string]string) { called = true },
	})
	dir := f.create("demo", testkit.NewRepo(t))
	f.states.Update(filepath.Join(dir, "state.json"), func(s *state.State) error {
		s.Services["svc1"] = state.ServiceState{Agent: state.Agent{PaneID: "p1", State: domain.AgentIdle}}
		return nil
	})
	if _, err := f.setPhase("demo", domain.PhaseImplement, ""); err == nil {
		t.Fatal("moved without the plan approval")
	}
	if called {
		t.Error("an agent was told about a move that did not happen")
	}
}
