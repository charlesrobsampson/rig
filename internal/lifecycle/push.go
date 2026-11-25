package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/state"
)

// Push runs the push step of a ticket in the push phase: each service is published the
// way its policy says (none, push-branch, merge, pr), always from the user's main
// repository after the checkout's branch has been imported into it. Services that were
// pushed by an earlier run are left alone, so a run after a partial failure retries only
// what failed. The ticket moves to done only once every service has been pushed.
//
// Being in the push phase already means the review gate was approved, so no gate is
// checked here; a ticket in any other phase is refused.
func (m *Manager) Push(ctx context.Context, p api.PushRunParams) (api.PushRunResult, error) {
	l, unlock, err := m.begin(p.ID)
	if err != nil {
		return api.PushRunResult{}, err
	}
	defer unlock()

	switch l.state.Phase {
	case domain.PhasePush:
	case domain.PhaseDone:
		return api.PushRunResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("ticket %q is done: everything was pushed", p.ID)}
	default:
		return api.PushRunResult{}, &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf(
			"ticket %q is in phase %s; pushing happens in the push phase (move there with: rig phase %s push, which needs the review approval)", p.ID, l.state.Phase, p.ID)}
	}
	if p.Service != "" && !hasService(l.ticket, p.Service) {
		return api.PushRunResult{}, &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no service %q", p.ID, p.Service)}
	}

	var res api.PushRunResult
	for _, svc := range l.ticket.Services {
		if p.Service != "" && svc.Name != p.Service {
			continue
		}
		if prior := l.state.Services[svc.Name].Push; prior.Status == domain.PushDone {
			res.Services = append(res.Services, api.PushServiceResult{
				Service: svc.Name, Mode: policy.Resolve(m.opts.Global, l.ticket, svc, policy.Flags{}).PushMode,
				Status: domain.PushDone, Skipped: true, PRURL: prior.PRURL, Note: "already pushed by an earlier run",
			})
			continue
		}
		// Measured before pushing, while the checkout is surely there; kept for rig stats.
		rec := l.state.Services[svc.Name]
		change, _ := m.liveChange(ctx, svc, rec.Checkout.Path, rec.Checkout.Kind, rec.Base)
		out := m.pushService(ctx, l, svc)
		record := state.Push{Status: out.Status, PRURL: out.PRURL, Error: out.Error, Files: change.Files, Added: change.Added, Deleted: change.Deleted}
		if err := m.opts.States.SetPush(l.statePath, svc.Name, record); err != nil {
			out.Status, out.Error = domain.PushFailed, fmt.Sprintf("record the result: %v", err)
		}
		res.Services = append(res.Services, out)
	}

	// done only when every service of the ticket (not just those this run touched) is pushed
	fresh, err := m.opts.States.Load(l.statePath)
	if err != nil {
		return api.PushRunResult{}, &api.Error{Code: api.CodeInternal, Message: err.Error()}
	}
	all := true
	for _, svc := range l.ticket.Services {
		all = all && fresh.Services[svc.Name].Push.Status == domain.PushDone
	}
	if all {
		l.state = fresh
		if _, err := m.moveLocked(l, p.ID, "human", domain.PhaseDone, "", ""); err != nil {
			return api.PushRunResult{}, err
		}
		res.Done = true
	}
	summary, err := m.reg.Get(p.ID)
	if err != nil {
		return api.PushRunResult{}, err
	}
	res.Summary = summary.Summary
	return res, nil
}

func (m *Manager) pushService(ctx context.Context, l loaded, svc domain.Service) api.PushServiceResult {
	eff := policy.Resolve(m.opts.Global, l.ticket, svc, policy.Flags{})
	out := api.PushServiceResult{Service: svc.Name, Mode: eff.PushMode, Status: domain.PushFailed}
	fail := func(format string, args ...any) api.PushServiceResult {
		out.Error = fmt.Sprintf(format, args...)
		return out
	}
	if eff.PushMode == "" {
		return fail("no push mode is configured for this service (set push.mode to none, push-branch, merge or pr)")
	}

	rec := l.state.Services[svc.Name]
	if rec.Checkout.Path == "" {
		return fail("the service has no checkout (the ticket was stopped); start it again before pushing")
	}
	co, err := git.Open(ctx, git.Spec{Repo: svc.Repo, Branch: svc.Branch, Path: rec.Checkout.Path, Kind: rec.Checkout.Kind})
	if err != nil {
		return fail("open the checkout: %v", err)
	}
	// Only commits are pushed; uncommitted work would silently be left out.
	dirty, err := co.IsDirty(ctx)
	if err != nil {
		return fail("inspect the checkout: %v", err)
	}
	if dirty.Modified+dirty.Untracked > 0 {
		return fail("the checkout has %d uncommitted change(s) that would not be pushed; commit or discard them first (rig review shows them)", dirty.Modified+dirty.Untracked)
	}
	if err := co.Import(ctx, false); err != nil {
		return fail("import the branch into %s: %v", svc.Repo, err)
	}

	base := firstNonEmptyString(svc.Base, rec.Base)
	if base == "" {
		if base, err = git.DefaultBranch(ctx, svc.Repo); err != nil {
			return fail("%v", err)
		}
	}
	opts := git.PushOptions{Repo: svc.Repo, Branch: svc.Branch, Base: base, MergeCommit: eff.MergeCommit}

	var result git.PushResult
	if eff.PushMode == domain.PushPR {
		gh := m.opts.GH
		if gh == nil {
			gh = git.NewGH("")
		}
		content := m.prContent(ctx, l, svc, rec, base)
		switch {
		case content.Description == "" && content.Multi:
			return fail("pr.md has no description for %s: the agent writes it under \"## %s\" in the ticket's pr.md before the work is pushed", svc.Name, svc.Name)
		case content.Description == "":
			return fail("pr.md has no description: the agent writes the pull request description in the ticket's pr.md before the work is pushed")
		}
		if leak := m.prLeak(l, content.Description); leak != "" {
			return fail("pr.md mentions the local path %s; it is published as the pull request description, so it must not refer to this machine's files", leak)
		}
		result, err = git.PushPR(ctx, gh, git.PROptions{PushOptions: opts, GitHubUser: eff.GitHubUser, Title: git.PRTitle(content), Body: git.PRBody(content)})
	} else {
		result, err = git.Push(ctx, eff.PushMode, opts)
	}
	out.Pushed, out.Ref, out.Sha, out.PRURL, out.Note = result.Pushed, result.Ref, result.Sha, result.PRURL, result.Note
	if err != nil {
		return fail("%v", err)
	}
	out.Status = domain.PushDone
	return out
}

// ensurePRFile gives a ticket made before pr.md existed its stub, so a container agent
// can be given the file to write; an existing pr.md is never touched.
func ensurePRFile(l loaded) error {
	path := filepath.Join(l.dir, "pr.md")
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var names []string
	for _, s := range l.ticket.Services {
		names = append(names, s.Name)
	}
	return os.WriteFile(path, []byte(git.PRStub(names)), 0o600)
}

// prLeak names a local path the description mentions, or "": a PR body is published,
// and these only make sense on this machine (or show what is private on it).
func (m *Manager) prLeak(l loaded, description string) string {
	user, _ := os.UserHomeDir()
	for _, p := range []string{m.opts.Dir, m.opts.Global.Paths.Home, user, agentTicketDir} {
		if p != "" && strings.Contains(description, p) {
			return p
		}
	}
	return ""
}

// prContent is what the pull request description is built from.
func (m *Manager) prContent(ctx context.Context, l loaded, svc domain.Service, rec state.ServiceState, base string) git.PRContent {
	c := git.PRContent{TicketID: l.id, Title: l.ticket.Title, Service: svc.Name, Multi: len(l.ticket.Services) > 1}
	if l.ticket.Source.Type != "" && l.ticket.Source.Type != "manual" {
		c.Source = l.ticket.Source.Type + " " + l.ticket.Source.Key
	}
	if raw, err := os.ReadFile(filepath.Join(l.dir, "pr.md")); err == nil {
		c.Description = git.PRDescription(string(raw), svc.Name, c.Multi)
	}
	baseRef := base
	if rec.Checkout.Kind == domain.CheckoutClone {
		baseRef = "origin/" + base
	}
	if diff, err := git.Diff(ctx, rec.Checkout.Path, baseRef); err == nil {
		c.Files = diff.Files
	}
	return c
}
