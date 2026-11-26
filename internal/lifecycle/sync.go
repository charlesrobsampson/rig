package lifecycle

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

// baseOf is the branch a service's work is measured and merged against: its own, the
// one recorded at start, else the repository's default (as rig push decides).
func baseOf(ctx context.Context, svc domain.Service, rec state.ServiceState) (string, error) {
	if b := firstNonEmptyString(svc.Base, rec.Base); b != "" {
		return b, nil
	}
	return git.DefaultBranch(ctx, svc.Repo)
}

// BaseStatus lists the services of a ticket whose base has moved on since they branched,
// with the files a merge would conflict in. Only tickets with work under way are looked
// at; anything that cannot be measured is left out rather than guessed.
func (m *Manager) BaseStatus(ctx context.Context, t domain.Ticket, st state.State) []api.BaseInfo {
	if st.Phase == domain.PhaseDone {
		return nil
	}
	var out []api.BaseInfo
	for _, svc := range t.Services {
		rec := st.Services[svc.Name]
		if rec.Checkout.Path == "" {
			continue
		}
		if _, err := os.Stat(rec.Checkout.Path); err != nil {
			continue
		}
		base, err := baseOf(ctx, svc, rec)
		if err != nil {
			continue
		}
		bs, err := git.CompareBase(ctx, svc.Repo, rec.Checkout.Path, base)
		if err != nil || bs.Behind == 0 {
			continue
		}
		out = append(out, api.BaseInfo{Service: svc.Name, Base: bs.Base, Behind: bs.Behind, Conflicts: bs.Conflicts})
	}
	return out
}

// Touched lists, per service, the files its checkout has changed against its base,
// committed or not. Like BaseStatus it skips done tickets and anything it cannot
// measure, and a service with no changes is not listed.
func (m *Manager) Touched(ctx context.Context, t domain.Ticket, st state.State) []api.TouchedInfo {
	if st.Phase == domain.PhaseDone {
		return nil
	}
	var out []api.TouchedInfo
	for _, svc := range t.Services {
		rec := st.Services[svc.Name]
		if rec.Checkout.Path == "" {
			continue
		}
		if _, err := os.Stat(rec.Checkout.Path); err != nil {
			continue
		}
		base, err := baseOf(ctx, svc, rec)
		if err != nil {
			continue
		}
		files, err := git.Touched(ctx, rec.Checkout.Path, base)
		if err != nil || len(files) == 0 {
			continue
		}
		out = append(out, api.TouchedInfo{Service: svc.Name, Files: files})
	}
	return out
}

// identity is who rig's merge commits are by: the person the main repository is set up
// for, since they asked for the sync.
func identity(ctx context.Context, repo string) (name, email string) {
	name, _ = git.Trusted().Run(ctx, repo, "config", "user.name")
	email, _ = git.Trusted().Run(ctx, repo, "config", "user.email")
	if name == "" || email == "" {
		return "rig", "rig@localhost"
	}
	return name, email
}

// endLoopAfterSync drops the review loop of a reviewed service whose branch a sync
// changed: what it passed or found is not what the branch holds now, so the next
// review.ready starts a new loop with its own rounds. A loop whose round is running is
// left, because the round's verdict still has to land. A move back to implement has
// cleared the loop already, and then there is nothing to end.
func (m *Manager) endLoopAfterSync(id string) error {
	l, unlock, err := m.begin(id)
	if err != nil {
		return err
	}
	defer unlock()
	ended := l.state.Review
	if ended == nil || ended.Status == domain.ReviewReviewing {
		return nil
	}
	if err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		s.Review = nil
		return nil
	}); err != nil {
		return err
	}
	m.note(l.dir, timeline.ReviewLoopEnded, "human", "", timeline.ReviewLoopEndedData{Rounds: len(ended.Rounds), Status: string(ended.Status)})
	return nil
}

// Sync brings each service's base into its branch. A clean merge is committed, the
// coder is told, and a reviewed ticket starts a new review round on it; a conflict is
// left in the checkout for the coder to resolve, with the files named. A ticket past
// implement goes back to implement, because its reviewed work changed. Nothing is
// fetched: the base is the main repository's branch.
func (m *Manager) Sync(ctx context.Context, p api.SyncParams) (api.SyncResult, error) {
	l, unlock, err := m.begin(p.ID)
	if err != nil {
		return api.SyncResult{}, err
	}
	switch l.state.Phase {
	case domain.PhasePush, domain.PhaseDone:
		unlock()
		return api.SyncResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("ticket %q is in %s: its work is already being published, so it is not synced", p.ID, l.state.Phase)}
	}
	var res api.SyncResult
	for _, svc := range l.ticket.Services {
		if p.Service != "" && svc.Name != p.Service {
			continue
		}
		rec := l.state.Services[svc.Name]
		if rec.Checkout.Path == "" {
			unlock()
			return api.SyncResult{}, &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("service %s has no checkout; start the ticket first", svc.Name)}
		}
		co, err := git.Open(ctx, git.Spec{Repo: svc.Repo, Branch: svc.Branch, Path: rec.Checkout.Path, Kind: rec.Checkout.Kind})
		if err != nil {
			unlock()
			return api.SyncResult{}, err
		}
		if d, err := co.IsDirty(ctx); err != nil || d.Modified+d.Untracked > 0 {
			unlock()
			if err != nil {
				return api.SyncResult{}, err
			}
			return api.SyncResult{}, &api.Error{Code: api.CodeBlocked, Message: fmt.Sprintf("service %s has uncommitted work (%s); it is committed or discarded before a sync", svc.Name, d.Describe())}
		}
		base, err := baseOf(ctx, svc, rec)
		if err != nil {
			unlock()
			return api.SyncResult{}, err
		}
		bs, err := git.CompareBase(ctx, svc.Repo, rec.Checkout.Path, base)
		if err != nil {
			unlock()
			return api.SyncResult{}, err
		}
		s := api.SyncService{BaseInfo: api.BaseInfo{Service: svc.Name, Base: base, Behind: bs.Behind}}
		if bs.Behind > 0 {
			name, email := identity(ctx, svc.Repo)
			s.Merged, s.Conflicts, err = git.MergeBase(ctx, svc.Repo, rec.Checkout.Path, base, name, email)
			if err != nil {
				unlock()
				return api.SyncResult{}, err
			}
		}
		res.Services = append(res.Services, s)
		m.note(l.dir, timeline.SyncDone, "human", svc.Name, timeline.SyncDoneData{Service: svc.Name, Behind: s.Behind, Merged: s.Merged, Conflicts: s.Conflicts})
	}
	if p.Service != "" && len(res.Services) == 0 {
		unlock()
		return api.SyncResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no service %q", p.ID, p.Service)}
	}
	phase, reviewed := l.state.Phase, l.ticket.Review != nil
	unlock()

	changed := false
	for _, s := range res.Services {
		switch {
		case len(s.Conflicts) > 0:
			changed = true
			m.deliver(p.ID, s.Service, fmt.Sprintf("rig merged %s (%d new commits) into your branch, and these files conflict: %s. Resolve them so that your change and the work on %s both keep working (this merge is the one exception to never fetching or merging), commit the merge, run the tests, then journal review.ready: the earlier review no longer covers the merged branch, so it starts again from round 1.", s.Base, s.Behind, strings.Join(s.Conflicts, ", "), s.Base))
		case s.Merged != "":
			changed = true
			m.deliver(p.ID, s.Service, fmt.Sprintf("rig merged %s (%d new commits) into your branch as %s, so your work now sits on the current %s. Run the tests and fix anything the merge broke; a new review (round 1) checks the merged work.", s.Base, s.Behind, short(s.Merged), s.Base))
		}
	}
	res.Phase = phase
	if changed && (phase == domain.PhaseTest || phase == domain.PhaseReview) {
		if _, err := m.SetPhase(ctx, api.PhaseSetParams{ID: p.ID, To: domain.PhaseImplement, Reason: "synced with the base branch; the merged work is checked again"}); err != nil {
			return res, err
		}
		res.Phase = domain.PhaseImplement
	}
	if target, _, ok := reviewers(l.ticket); ok {
		var branchChanged bool
		for _, s := range res.Services {
			branchChanged = branchChanged || s.Service == target.Name && (s.Merged != "" || len(s.Conflicts) > 0)
		}
		// If the old loop cannot be ended, no round is started on it: it would go on at
		// the old round count, which is what ending it is for.
		if branchChanged {
			if err := m.endLoopAfterSync(p.ID); err != nil {
				return res, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("the branch was merged, but the review loop could not be ended (journal review.ready after resolving, or run rig reviewer %s --start): %v", p.ID, err)}
			}
		}
	}
	clean := changed
	for _, s := range res.Services {
		clean = clean && len(s.Conflicts) == 0
	}
	if clean && reviewed && res.Phase == domain.PhaseImplement {
		if err := m.beginRound(p.ID, "rig sync"); err == nil {
			res.ReviewStarted = true
			// beginRound only marks the round; the reviewer's copy and brief are made
			// here, as for a review.ready, or the loop would wait on a round nobody runs.
			m.bg.Add(1)
			go func() {
				defer m.bg.Done()
				m.runRound(m.bgCtx, p.ID)
			}()
		}
	}
	return res, nil
}
