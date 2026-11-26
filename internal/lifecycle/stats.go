package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/journal"
	"github.com/charlesrobsampson/rig/internal/timeline"
	"github.com/charlesrobsampson/rig/internal/usage"
)

// Stats is everything rig measured about the tickets asked for (all of them when none
// is named): cost and tokens per agent, time per phase and at each gate, review rounds
// and the size of each change, with totals for the vessel. A ticket that cannot be read
// is listed with its error, never left out.
func (m *Manager) Stats(ctx context.Context, p api.StatsParams) (api.StatsResult, error) {
	ids := p.IDs
	if len(ids) == 0 {
		entries, err := os.ReadDir(m.opts.Dir)
		if err != nil && !os.IsNotExist(err) {
			return api.StatsResult{}, err
		}
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(m.opts.Dir, e.Name(), "state.json")); e.IsDir() && err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	var res api.StatsResult
	for _, id := range ids {
		l, err := m.load(id)
		if err != nil {
			if len(p.IDs) > 0 {
				return api.StatsResult{}, err
			}
			res.Tickets = append(res.Tickets, api.TicketStats{ID: id, Error: err.Error()})
			continue
		}
		s := m.ticketStats(ctx, l)
		res.Tickets = append(res.Tickets, s)
		t := &res.Totals
		t.Tickets++
		if s.Phase == domain.PhaseDone {
			t.Done++
		}
		t.CostUSD += s.CostUSD
		t.Tokens = addTokens(t.Tokens, s.Tokens)
		t.ElapsedSeconds += s.ElapsedSeconds
		t.ReviewRounds += s.ReviewRounds
		for _, c := range s.Changes {
			t.Added, t.Deleted = t.Added+c.Added, t.Deleted+c.Deleted
		}
	}
	return res, nil
}

func addTokens(a, b api.TokenCount) api.TokenCount {
	return api.TokenCount{Input: a.Input + b.Input, CacheWrite: a.CacheWrite + b.CacheWrite, CacheRead: a.CacheRead + b.CacheRead, Output: a.Output + b.Output}
}

func (m *Manager) ticketStats(ctx context.Context, l loaded) api.TicketStats {
	st := l.state
	s := api.TicketStats{ID: l.id, Title: l.ticket.Title, Phase: st.Phase, CreatedAt: st.CreatedAt}

	// Every agent the ticket has, and any other that left a cost in state (a reviewer
	// from a review block since removed), so this and rig list add up the same.
	names := []string{}
	for _, svc := range agentServices(l.ticket) {
		names = append(names, svc.Name)
	}
	var others []string
	for name, ss := range st.Services {
		if ss.Agent.Cost != nil && !slices.Contains(names, name) {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	names = append(names, others...)
	for _, name := range names {
		a := api.AgentStats{Service: name, Role: "coder"}
		switch name {
		case domain.ReviewerService:
			a.Role = "reviewer"
		case domain.TesterService:
			a.Role = "tester"
		}
		svc := domain.Service{Name: name}
		if c := st.Services[svc.Name].Agent.Cost; c != nil {
			a.CostUSD = c.USD
		}
		if t, err := usage.ReadTranscripts(agentConfigDir(l.dir, svc.Name)); err == nil {
			a.Tokens = api.TokenCount(t.Tokens)
			a.Requests, a.Models = t.Requests, t.Models
		}
		s.Agents = append(s.Agents, a)
		s.CostUSD += a.CostUSD
		s.Tokens = addTokens(s.Tokens, a.Tokens)
	}

	// Phases: each transition ends the time spent in the phase it leaves.
	now := m.opts.Clock.Now()
	spent := map[domain.Phase]int64{}
	var order []domain.Phase
	add := func(p domain.Phase, from, to time.Time) {
		if p == "" || p == domain.PhaseDone || from.IsZero() || to.Before(from) {
			return
		}
		if _, ok := spent[p]; !ok {
			order = append(order, p)
		}
		spent[p] += int64(to.Sub(from).Seconds())
	}
	since := st.CreatedAt
	for _, tr := range st.Transitions {
		add(tr.From, since, tr.At)
		since = tr.At
		if tr.To == domain.PhaseDone && st.Phase == domain.PhaseDone {
			at := tr.At
			s.DoneAt = &at
		}
	}
	add(st.Phase, since, now)
	for _, p := range order {
		s.Phases = append(s.Phases, api.PhaseTime{Phase: p, Seconds: spent[p]})
	}
	end := now
	if s.DoneAt != nil {
		end = *s.DoneAt
	}
	if !st.CreatedAt.IsZero() {
		s.ElapsedSeconds = int64(end.Sub(st.CreatedAt).Seconds())
	}

	// Gates: from the last request to move on to the approval that answered it.
	records, _ := journal.Read(l.dir)
	var after time.Time
	for _, a := range st.Approvals {
		var asked time.Time
		for _, r := range records {
			if r.Kind == "transition.request" && r.At.After(after) && !r.At.After(a.At) {
				asked = r.At
			}
		}
		if !asked.IsZero() {
			w := int64(a.At.Sub(asked).Seconds())
			s.Gates = append(s.Gates, api.GateWait{Gate: a.Gate, Seconds: w})
			s.WaitingOnPeopleSeconds += w
		}
		after = a.At
	}

	// Rounds of every loop, as the timeline kept them (state.json holds only the current
	// loop, R-024). A ticket older than its timeline, or a round whose start failed
	// before it was recorded there, is still counted from state.
	if entries, err := timeline.Read(l.dir, 0); err == nil {
		for _, e := range entries {
			if e.Kind == timeline.ReviewRoundStarted {
				s.ReviewRounds++
			}
		}
	}
	if r := st.Review; r != nil {
		s.ReviewRounds, s.ReviewStatus = max(s.ReviewRounds, len(r.Rounds)), r.Status
	}

	for _, svc := range l.ticket.Services {
		rec := st.Services[svc.Name]
		c, ok := m.liveChange(ctx, svc, rec.Checkout.Path, rec.Checkout.Kind, rec.Base)
		if !ok && rec.Push.Files > 0 {
			c, ok = api.ChangeStats{Files: rec.Push.Files, Added: rec.Push.Added, Deleted: rec.Push.Deleted}, true
		}
		if ok {
			if s.Changes == nil {
				s.Changes = map[string]api.ChangeStats{}
			}
			s.Changes[svc.Name] = c
		}
	}
	return s
}

// liveChange measures a service's change in its checkout, while there is one.
func (m *Manager) liveChange(ctx context.Context, svc domain.Service, path string, kind domain.CheckoutKind, recordedBase string) (api.ChangeStats, bool) {
	if path == "" {
		return api.ChangeStats{}, false
	}
	base := firstNonEmptyString(svc.Base, recordedBase)
	if base == "" {
		var err error
		if base, err = git.DefaultBranch(ctx, svc.Repo); err != nil { // as rig push does
			return api.ChangeStats{}, false
		}
	}
	if kind == domain.CheckoutClone {
		base = "origin/" + base
	}
	d, err := git.Diff(ctx, path, base)
	if err != nil {
		return api.ChangeStats{}, false
	}
	return changeOf(d.Files), true
}

func changeOf(files []git.FileChange) api.ChangeStats {
	c := api.ChangeStats{Files: len(files)}
	for _, f := range files {
		c.Added, c.Deleted = c.Added+f.Added, c.Deleted+f.Deleted
	}
	return c
}
