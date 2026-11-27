package lifecycle_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/journal"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

func TestStatsMeasureAgentsPhasesGatesAndChangesAndAddUpTheVessel(t *testing.T) {
	dir := filepath.Join(testkit.RealTempDir(t), "tickets")
	clock := testkit.NewFakeClock(epoch)
	states := state.NewManager(clock)
	gates := []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval}
	lc := lifecycle.New(lifecycle.Options{Dir: dir, States: states, Bus: events.NewBus(clock, 0), Clock: clock,
		Global: config.Global{Defaults: config.Defaults{BranchPrefix: "rig/", Policy: domain.Policy{Gates: gates}}}})
	creator := registry.NewCreator(registry.CreatorOptions{Dir: dir, States: states, Sources: ticket.DefaultRegistry(),
		RepoCheck: func(p string) error { return git.CheckRepo(ctx, p) }, BranchPrefix: "rig/"})
	res, err := creator.Create(ctx, api.TicketNewParams{ID: "demo", Title: "T demo", Services: []api.ServiceSpec{{Name: "svc1", Repo: testkit.NewRepo(t).Dir}}})
	if err != nil {
		t.Fatal(err)
	}
	path := res.Path
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(mustRead(t, filepath.Join(path, "ticket.yaml")), []byte("review: {}\n")...), 0o600)

	at := func(minutes int) time.Time { return epoch.Add(time.Duration(minutes) * time.Minute) }
	ask := func(minutes int) {
		journal.Append(path, api.JournalRecord{At: at(minutes), Ticket: "demo", Service: "svc1", Source: api.JournalSourceContainer, Kind: "transition.request"})
	}
	move := func(minutes int, to domain.Phase, gate domain.Gate) {
		t.Helper()
		clock.Advance(at(minutes).Sub(clock.Now()))
		if gate != "" {
			if _, err := lc.Approve(ctx, api.GateApproveParams{ID: "demo", Gate: gate}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := lc.SetPhase(ctx, api.PhaseSetParams{ID: "demo", To: to}); err != nil {
			t.Fatal(err)
		}
	}
	ask(10)
	move(15, domain.PhaseImplement, domain.GatePlanApproval)
	states.Update(filepath.Join(path, "state.json"), func(s *state.State) error {
		s.Review = &state.Review{Status: domain.ReviewPassed, Round: 2, Rounds: []state.ReviewRound{{Round: 1}, {Round: 2}}}
		return nil
	})
	move(45, domain.PhaseTest, "")
	move(50, domain.PhaseReview, "")
	ask(55)
	move(60, domain.PhasePush, domain.GateReviewApproval)
	move(61, domain.PhaseDone, "")
	clock.Advance(at(500).Sub(clock.Now())) // later reads must not stretch a finished ticket

	states.Update(filepath.Join(path, "state.json"), func(s *state.State) error {
		for name, usd := range map[string]float64{"svc1": 0.5, "review": 0.25} {
			ss := s.Services[name]
			ss.Agent.Cost = &state.Cost{USD: usd}
			ss.Push = state.Push{Status: domain.PushDone, Files: 3, Added: 10, Deleted: 2}
			s.Services[name] = ss
		}
		return nil
	})
	transcript := `{"type":"user","timestamp":"2026-11-09T12:10:00Z"}` + "\n" + `{"type":"assistant","timestamp":"2026-11-09T12:20:00Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":50}}}` + "\n"
	for _, svc := range []string{"svc1", "review"} {
		p := filepath.Join(path, ".rig-claude", svc, "projects", "-work", "s.jsonl")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(transcript), 0o600)
	}
	os.MkdirAll(filepath.Join(dir, "broken"), 0o700)
	os.WriteFile(filepath.Join(dir, "broken", "state.json"), []byte("{"), 0o600)

	got, err := lc.Stats(ctx, api.StatsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tickets) != 2 {
		t.Fatalf("tickets = %+v", got.Tickets)
	}
	var s api.TicketStats
	for _, ts := range got.Tickets {
		if ts.ID == "demo" {
			s = ts
		} else if ts.Error == "" {
			t.Errorf("the unreadable ticket has no error: %+v", ts)
		}
	}
	if s.CostUSD != 0.75 || len(s.Agents) != 2 || s.Agents[1].Role != "reviewer" || s.Agents[0].Requests != 1 || s.Agents[0].Models[0] != "claude-sonnet-5-5" {
		t.Errorf("agents = %+v, cost %v", s.Agents, s.CostUSD)
	}
	if s.AgentSeconds != 1200 || s.Agents[0].AgentSeconds != 600 || got.Totals.AgentSeconds != 1200 {
		t.Errorf("agent seconds: ticket %d, svc1 %d, totals %d; want 1200, 600, 1200", s.AgentSeconds, s.Agents[0].AgentSeconds, got.Totals.AgentSeconds)
	}
	if s.Tokens != (api.TokenCount{Input: 20, CacheWrite: 200, CacheRead: 2000, Output: 100}) {
		t.Errorf("tokens = %+v", s.Tokens)
	}
	wantPhases := []api.PhaseTime{{Phase: domain.PhasePlan, Seconds: 900}, {Phase: domain.PhaseImplement, Seconds: 1800}, {Phase: domain.PhaseTest, Seconds: 300}, {Phase: domain.PhaseReview, Seconds: 600}, {Phase: domain.PhasePush, Seconds: 60}}
	if len(s.Phases) != len(wantPhases) {
		t.Fatalf("phases = %+v", s.Phases)
	}
	for i, p := range wantPhases {
		if s.Phases[i] != p {
			t.Errorf("phase %d = %+v, want %+v", i, s.Phases[i], p)
		}
	}
	if s.ElapsedSeconds != 3660 || s.DoneAt == nil || !s.DoneAt.Equal(at(61)) {
		t.Errorf("elapsed %d, done %v", s.ElapsedSeconds, s.DoneAt)
	}
	if s.WaitingOnPeopleSeconds != 600 || len(s.Gates) != 2 || s.Gates[0] != (api.GateWait{Gate: domain.GatePlanApproval, Seconds: 300}) {
		t.Errorf("waiting %d, gates %+v", s.WaitingOnPeopleSeconds, s.Gates)
	}
	if s.ReviewRounds != 2 || s.ReviewStatus != domain.ReviewPassed || s.Changes["svc1"] != (api.ChangeStats{Files: 3, Added: 10, Deleted: 2}) {
		t.Errorf("review %d %s, changes %+v", s.ReviewRounds, s.ReviewStatus, s.Changes)
	}
	tot := got.Totals
	if tot.Tickets != 1 || tot.Done != 1 || tot.CostUSD != 0.75 || tot.Tokens != s.Tokens || tot.ElapsedSeconds != 3660 || tot.ReviewRounds != 2 || tot.Added != 10 {
		t.Errorf("totals = %+v", tot)
	}
	tk, _ := config.LoadTicket(filepath.Join(path, "ticket.yaml"), config.TicketOptions{RepoCheck: func(string) error { return nil }})
	st, _ := states.Load(filepath.Join(path, "state.json"))
	if n := lc.AgentSeconds(tk, st); n != s.AgentSeconds {
		t.Errorf("the summary's agent seconds = %d, rig stats says %d", n, s.AgentSeconds)
	}
	// cached: with the signature unchanged the files are not read again (an unreadable
	// file would make a re-read fail or differ); a changed transcript is picked up
	p := filepath.Join(path, ".rig-claude", "svc1", "projects", "-work", "s.jsonl")
	if n := lc.AgentSeconds(tk, st); n != 1200 {
		t.Errorf("cached = %d", n)
	}
	os.WriteFile(p, append(mustRead(t, p), []byte(`{"type":"user","timestamp":"2026-11-09T12:25:00Z"}`+"\n")...), 0o600)
	if n := lc.AgentSeconds(tk, st); n != 1200+300 {
		t.Errorf("after the transcript grew = %d, want 1500", n)
	}
	if _, err := lc.Stats(ctx, api.StatsParams{IDs: []string{"broken"}}); err == nil {
		t.Error("a named ticket that cannot be read was not an error")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
