package lifecycle

import (
	"context"
	"errors"
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

// endLoopAfterSync ends the review loop of a reviewed service whose branch a sync
// changed: what it passed or found is not what the branch holds now, so the review
// waits (awaiting_ready) and the next review.ready or --start begins a new loop. A loop whose round is running is
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
	if ended.Status == domain.ReviewAwaitingReady {
		// Already waiting. A lost round (never pinned) would continue its loop, but the
		// merged branch is not what any earlier round saw: drop it so the next round
		// starts a new loop. After an earlier sync every round has a commit; nothing to do.
		if n := len(ended.Rounds); n > 0 && ended.Rounds[n-1].Commit == "" {
			m.setReview(l, func(r *state.Review) {
				r.Rounds, r.Round = r.Rounds[:n-1], r.Round-1
			})
			if now, err := m.opts.States.Load(l.statePath); err != nil || now.Review == nil || len(now.Review.Rounds) != n-1 {
				return errors.New("the lost review round could not be dropped")
			}
			m.note(l.dir, timeline.ReviewLoopEnded, "human", "", timeline.ReviewLoopEndedData{Rounds: n - 1, Status: string(ended.Status)})
		}
		return nil
	}
	// The loop is over, but the next round must still be startable: the review waits
	// for review.ready (or --start) with its round number kept and no commit pinned.
	m.setReview(l, func(r *state.Review) {
		r.Status, r.Commit, r.Base, r.Reason, r.By = domain.ReviewAwaitingReady, "", "", "", ""
	})
	// setReview reports nothing back; read the state to know the change was made.
	if now, err := m.opts.States.Load(l.statePath); err != nil || now.Review == nil || now.Review.Status != domain.ReviewAwaitingReady {
		return errors.New("the review could not be marked as waiting for review.ready")
	}
	m.note(l.dir, timeline.ReviewLoopEnded, "human", "", timeline.ReviewLoopEndedData{Rounds: len(ended.Rounds), Status: string(ended.Status)})
	return nil
}

// Sync brings each service's base into its branch. A clean merge is committed, the
// coder is told, and a reviewed ticket starts a new review round on it; a conflict is
// left in the checkout for the coder to resolve, with the files named. A ticket past
// implement goes back to implement, because its reviewed work changed; a round that passes on the
// merged work brings it back (returnAfterSync). Nothing is
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
		if reviewed {
			// After the move, which clears it: only a passing round brings the ticket back.
			err := m.opts.States.Update(l.statePath, func(s *state.State) error {
				s.SyncReturn = phase
				return nil
			})
			if err != nil {
				return res, fmt.Errorf("record the phase to return to: %w", err)
			}
		}
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
		if err := m.beginRound(p.ID, "rig sync"); err != nil {
			// The review waits as awaiting_ready; say why, so the coder can start it.
			// (A round that is already running is left alone, silently.)
			now, _ := m.load(p.ID)
			if target, _, ok := reviewers(l.ticket); ok && (now.state.Review == nil || now.state.Review.Status != domain.ReviewReviewing) {
				m.deliver(p.ID, target.Name, "The review round on the merged work did not start: "+err.Error()+". Fix that, commit, then journal review.ready.")
			}
		} else {
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

// returnAfterSync moves a ticket that a sync sent back to implement forward to the phase
// it left, once a round has passed on the merged work. It steps through each phase in
// between as a person's moves would, but checks no gate and grants nothing: the
// approvals the sync invalidated stay invalidated. The ticket's lock is held.
func (m *Manager) returnAfterSync(l loaded) {
	var made []state.Transition
	err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		made = nil
		if s.SyncReturn == "" || s.Phase != domain.PhaseImplement || s.Review == nil || s.Review.Status != domain.ReviewPassed {
			return nil
		}
		now := m.opts.Clock.Now().UTC()
		for s.Phase != s.SyncReturn {
			next, ok := state.NextPhase(s.Phase)
			if !ok || state.IsBackward(s.Phase, s.SyncReturn) {
				break
			}
			t := state.Transition{From: s.Phase, To: next, By: "rig", Reason: "review passed on the merged work", At: now}
			s.Transitions = append(s.Transitions, t)
			made = append(made, t)
			s.Phase = next
		}
		s.PhaseRequest, s.SyncReturn = nil, ""
		return nil
	})
	if err != nil {
		return
	}
	for _, t := range made {
		m.announceMove(l, t, "")
	}
}
