package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/journal"
	"github.com/charlesrobsampson/rig/internal/policy"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

// The record kinds of a review loop (docs/formats.md §14).
const (
	ReviewReadyKind  = "review.ready"
	ReviewResultKind = "review.result"
	ReviewReplyKind  = "review.reply"
	VerdictKind      = "verdict"
)

var severities = []string{"blocking", "should-fix", "nit"}

// reviewers returns the reviewed service and the reviewer as a service its agent can be
// launched as: the reviewed service's repository and toolchain, the review's own role,
// always in a container.
func reviewers(t domain.Ticket) (target, reviewer domain.Service, ok bool) {
	if t.Review == nil {
		return domain.Service{}, domain.Service{}, false
	}
	for _, s := range t.Services {
		if s.Name == t.Review.Of {
			target = s
		}
	}
	if target.Name == "" {
		return domain.Service{}, domain.Service{}, false
	}
	reviewer = domain.Service{
		Name: domain.ReviewerService, Repo: target.Repo, Branch: target.Branch, Base: target.Base,
		Path: "./" + domain.ReviewerService, Build: target.Build, Sandbox: domain.SandboxContainer,
		Harness: target.Harness, Auth: target.Auth, Egress: target.Egress,
		AgentConfig: t.Review.AgentConfig, Model: t.Review.Model,
	}
	return target, reviewer, true
}

// agentServices are the services that may have an agent: the ticket's own, and its
// reviewer.
func agentServices(t domain.Ticket) []domain.Service {
	if _, reviewer, ok := reviewers(t); ok {
		return append(append([]domain.Service(nil), t.Services...), reviewer)
	}
	return t.Services
}

func reviewerDir(ticketDir string) string { return filepath.Join(ticketDir, domain.ReviewerService) }

// Wait blocks until the review rounds started in the background have finished.
func (m *Manager) Wait() { m.bg.Wait() }

// Close stops background work and waits for it.
func (m *Manager) Close() {
	m.bgCancel()
	m.bg.Wait()
}

func (m *Manager) deliver(ticket, service, text string) {
	if m.opts.Deliver != nil {
		m.opts.Deliver(ticket, service, text)
	}
}

// ReviewRecord acts on the review records agents leave (the supervisor hands them on).
// An error makes the record rejected; when the agent must know, it is also told.
func (m *Manager) ReviewRecord(ticket, service, kind, text string, data json.RawMessage) error {
	if kind != ReviewReadyKind && kind != ReviewResultKind && kind != ReviewReplyKind && kind != VerdictKind {
		return nil
	}
	l, err := m.load(ticket)
	if err != nil {
		return err
	}
	target, _, ok := reviewers(l.ticket)
	if !ok {
		if kind == ReviewReadyKind {
			return errors.New("this ticket has no reviewer; ask for the next phase instead")
		}
		return nil
	}
	fromCoder, fromReviewer := service == target.Name, service == domain.ReviewerService
	switch {
	case kind == ReviewReadyKind && fromCoder:
		if err := m.beginRound(ticket, "agent:"+service); err != nil {
			m.deliver(ticket, service, "Your review.ready was not taken: "+err.Error())
			return err
		}
		m.bg.Add(1)
		go func() {
			defer m.bg.Done()
			m.runRound(m.bgCtx, ticket)
		}()
		return nil
	case kind == VerdictKind && fromReviewer:
		return m.closeRound(ticket, data)
	case kind == ReviewResultKind && fromReviewer:
		_, err := parseFinding(data)
		return err
	case kind == ReviewReplyKind && fromCoder:
		_, err := parseReply(data)
		return err
	}
	return fmt.Errorf("a %s record comes from the %s agent, not %s", kind, map[bool]string{true: "reviewer's", false: target.Name + " service's"}[kind == VerdictKind || kind == ReviewResultKind], service)
}

func parseFinding(data json.RawMessage) (api.ReviewFinding, error) {
	var f api.ReviewFinding
	if err := json.Unmarshal(data, &f); err != nil || f.ID == "" || f.What == "" {
		return f, errors.New(`a review.result needs data like {"id": "F1", "severity": "blocking", "file": "path", "line": 12, "what": "...", "scenario": "...", "fix": "..."}`)
	}
	if !contains(severities, f.Severity) {
		return f, fmt.Errorf("severity %q is not one of %s", f.Severity, strings.Join(severities, ", "))
	}
	return f, nil
}

func parseReply(data json.RawMessage) (api.ReviewReply, error) {
	var r api.ReviewReply
	if err := json.Unmarshal(data, &r); err != nil || r.ID == "" {
		return r, errors.New(`a review.reply needs data like {"id": "F1", "status": "fixed", "commit": "<sha>", "reason": "..."}`)
	}
	if !contains([]string{"fixed", "disagree", "question"}, r.Status) {
		return r, fmt.Errorf("status %q is not one of fixed, disagree, question", r.Status)
	}
	return r, nil
}

// beginRound decides whether a round may start and marks it started, so a second
// review.ready cannot start another. A cap or a dispute stops the loop instead.
func (m *Manager) beginRound(id, by string) error {
	l, unlock, err := m.begin(id)
	if err != nil {
		return err
	}
	defer unlock()
	target, _, ok := reviewers(l.ticket)
	if !ok {
		return invalid("ticket %q has no review in its ticket.yaml", id)
	}
	if l.state.Phase != domain.PhaseImplement {
		return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("a review round runs in implement; the ticket is in %s", l.state.Phase)}
	}
	r := l.state.Review
	human := !strings.HasPrefix(by, "agent:")
	switch {
	case r != nil && r.Status == domain.ReviewReviewing:
		return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("review round %d is still running; wait for its findings", r.Round)}
	case r != nil && r.Status == domain.ReviewStopped && !human:
		return &api.Error{Code: api.CodeConflict, Message: "the review loop was stopped (" + r.Reason + "); a person decides what happens next"}
	}
	rec := l.state.Services[target.Name].Checkout
	if rec.Path == "" {
		return &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("service %s has no checkout; start the ticket first", target.Name)}
	}
	co, err := git.Open(context.Background(), git.Spec{Repo: target.Repo, Branch: target.Branch, Path: rec.Path, Kind: rec.Kind})
	if err != nil {
		return err
	}
	if dirty, err := co.IsDirty(context.Background()); err != nil {
		return err
	} else if dirty.Modified+dirty.Untracked > 0 {
		dirty.Unimported = 0
		return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("service %s has uncommitted work (%s); commit it, the reviewer sees commits only", target.Name, dirty.Describe())}
	}

	spent := agentsCost(l.state)
	// From awaiting_ready the old loop is over (a sync ended it, or its round was lost),
	// so the round starts a new one; the end was logged when it happened.
	awaiting := r != nil && r.Status == domain.ReviewAwaitingReady
	newLoop := r == nil || awaiting || (r.Status == domain.ReviewStopped && human)
	if !newLoop {
		if reason := m.stopReason(l, target.Name, *r, spent); reason != "" {
			m.setReview(l, func(r *state.Review) { r.Status, r.Reason, r.By = domain.ReviewStopped, reason, "rig" })
			m.note(l.dir, timeline.ReviewDecided, "rig", "", timeline.ReviewDecidedData{Outcome: "stop", Reason: reason})
			m.deliver(id, target.Name, "The review loop stopped: "+reason+". Wait for a person.")
			return nil
		}
	}
	if newLoop && r != nil && !awaiting {
		m.note(l.dir, timeline.ReviewLoopEnded, by, "", timeline.ReviewLoopEndedData{Rounds: len(r.Rounds), Status: string(r.Status)})
	}
	m.setReview(l, func(r *state.Review) {
		if newLoop {
			*r = state.Review{CostAtStart: spent}
		}
		r.Status, r.Reason, r.By = domain.ReviewReviewing, "", ""
		r.Round++
		r.Rounds = append(r.Rounds, state.ReviewRound{Round: r.Round, At: m.opts.Clock.Now().UTC()})
	})
	m.setPending(id, true) // until runRound has pinned the round or given it up
	return nil
}

// stopReason says why the loop must not run another round: the round cap, the budget,
// or the coder disagreeing with a blocking finding, which a person must settle.
func (m *Manager) stopReason(l loaded, coder string, r state.Review, spent float64) string {
	cfg := l.ticket.Review
	if r.Round >= cfg.MaxRounds && r.Status != domain.ReviewPassed {
		return fmt.Sprintf("the round cap (%d) was reached with findings still open", cfg.MaxRounds)
	}
	if cfg.BudgetUSD > 0 && spent-r.CostAtStart >= cfg.BudgetUSD {
		return fmt.Sprintf("the budget of $%.2f for this loop was spent ($%.2f)", cfg.BudgetUSD, spent-r.CostAtStart)
	}
	if r.Status != domain.ReviewFixing || len(r.Rounds) == 0 {
		return ""
	}
	records, _ := journal.Read(l.dir)
	since := r.Rounds[len(r.Rounds)-1].At
	findings, replies := roundRecords(records, coder, since, time.Time{})
	for _, f := range findings {
		if rep, ok := replies[f.ID]; ok && f.Severity == "blocking" && rep.Status == "disagree" {
			return fmt.Sprintf("the coder disagrees with blocking finding %s (%s): %s", f.ID, f.What, rep.Reason)
		}
	}
	return ""
}

// runRound does the slow part of a round: the reviewer's copy, its agent, and the
// messages. A failure stops the loop, saying why, rather than leaving it hanging.
func (m *Manager) runRound(ctx context.Context, id string) {
	defer m.setPending(id, false)
	l, unlock, err := m.begin(id)
	if err != nil {
		return
	}
	defer unlock()
	target, reviewer, _ := reviewers(l.ticket)
	r := l.state.Review
	if r == nil || r.Status != domain.ReviewReviewing {
		return
	}
	fail := func(err error) {
		if ctx.Err() != nil {
			// rig is shutting down, not the round failing: leave it to be started again.
			m.setReview(l, func(r *state.Review) { r.Status, r.Commit, r.Base = domain.ReviewAwaitingReady, "", "" })
			return
		}
		reason := "round " + fmt.Sprint(r.Round) + " could not start: " + err.Error()
		m.setReview(l, func(r *state.Review) { r.Status, r.Reason, r.By = domain.ReviewStopped, reason, "rig" })
		m.note(l.dir, timeline.ReviewDecided, "rig", "", timeline.ReviewDecidedData{Outcome: "stop", Reason: reason})
		m.deliver(id, target.Name, "The review loop stopped: "+reason+". Wait for a person.")
	}
	dir := reviewerDir(l.dir)
	coderPath := l.state.Services[target.Name].Checkout.Path
	sha, err := git.SnapshotForReview(ctx, target.Repo, coderPath, target.Branch, id, dir)
	if err != nil {
		fail(err)
		return
	}
	base := m.reviewBase(ctx, target, sha)
	for _, rel := range l.ticket.Review.Copy {
		if err := copyIgnored(ctx, coderPath, dir, rel); err != nil {
			fail(fmt.Errorf("copy %s: %w", rel, err))
			return
		}
	}
	if err := m.ensureReviewer(ctx, l, reviewer); err != nil {
		fail(fmt.Errorf("start the reviewer: %w", err))
		return
	}
	m.setReview(l, func(r *state.Review) {
		r.Commit, r.Base = sha, base
		r.Rounds[len(r.Rounds)-1].Commit = sha
	})
	m.note(l.dir, timeline.ReviewRoundStarted, "rig", "", timeline.ReviewRoundStartedData{Round: r.Round, Commit: sha})
	m.deliver(id, domain.ReviewerService, m.roundBrief(l, *r, sha, base, target.Name))
	m.deliver(id, target.Name, fmt.Sprintf("Review round %d started on your commit %s. Wait for the findings; what you commit meanwhile is reviewed next round.", r.Round, short(sha)))
}

// reviewBase is where the change under review starts: the merge base with the service's
// base branch. Without one the reviewer is pointed at the commit alone.
func (m *Manager) reviewBase(ctx context.Context, svc domain.Service, sha string) string {
	base := svc.Base
	if base == "" {
		var err error
		if base, err = git.DefaultBranch(ctx, svc.Repo); err != nil {
			return ""
		}
	}
	out, err := git.Trusted().Run(ctx, svc.Repo, "merge-base", base, sha)
	if err != nil {
		return ""
	}
	return out
}

func (m *Manager) roundBrief(l loaded, r state.Review, sha, base, coder string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review round %d of at most %d. Your working directory now holds the coder's commit %s", r.Round, l.ticket.Review.MaxRounds, sha)
	if base != "" {
		fmt.Fprintf(&b, "; the change is `git diff %s %s`", base, sha)
	}
	b.WriteString(".")
	if r.Round > 1 && len(r.Rounds) > 1 {
		records, _ := journal.Read(l.dir)
		prev := r.Rounds[len(r.Rounds)-2]
		findings, replies := roundRecords(records, coder, prev.At, r.Rounds[len(r.Rounds)-1].At)
		if len(findings) > 0 {
			b.WriteString("\n\nYour findings from the last round, and the coder's answers:")
			for _, f := range findings {
				fmt.Fprintf(&b, "\n- %s (%s) %s: ", f.ID, f.Severity, f.What)
				if rep, ok := replies[f.ID]; ok {
					b.WriteString(rep.Status)
					if rep.Commit != "" {
						b.WriteString(" in " + short(rep.Commit))
					}
					if rep.Reason != "" {
						b.WriteString(": " + rep.Reason)
					}
				} else {
					b.WriteString("no answer")
				}
			}
			b.WriteString("\nCheck each again (re-run what proves it; do not take the answer on trust).")
		}
	}
	if told := peopleToCoder(l.dir, coder); len(told) > 0 {
		b.WriteString("\n\nWhat people told the coder during this ticket (decisions that may not be in plan.md yet; they stand unless they break the project's rules):")
		for _, m := range told {
			fmt.Fprintf(&b, "\n- %s: %s", m.At.Format("2006-01-02 15:04"), m.Text)
		}
	}
	fmt.Fprintf(&b, "\n\nJournal a review.result only for findings still open or new (keep the ids), then a verdict with data {\"round\": %d}.", r.Round)
	return b.String()
}

// toldMessage is one line of the ticket's message log (written by the injector).
type toldMessage struct {
	At        time.Time `json:"at"`
	Service   string    `json:"service"`
	From      string    `json:"from"`
	Text      string    `json:"text"`
	Delivered bool      `json:"delivered"`
}

// maxTold keeps the most recent messages when a ticket has had very many.
const maxTold = 30

// peopleToCoder is what a person (not rig) told the coder and the coder received: a
// reviewer that cannot see an answer given by message takes the coder following it for
// a deviation from the ticket.
func peopleToCoder(ticketDir, coder string) []toldMessage {
	raw, err := os.ReadFile(filepath.Join(ticketDir, "messages.jsonl"))
	if err != nil {
		return nil
	}
	var out []toldMessage
	for _, line := range strings.Split(string(raw), "\n") {
		var m toldMessage
		if json.Unmarshal([]byte(line), &m) != nil || m.Service != coder || m.From == "rig" || !m.Delivered {
			continue
		}
		out = append(out, m)
	}
	if len(out) > maxTold {
		out = out[len(out)-maxTold:]
	}
	return out
}

// closeRound takes the reviewer's verdict: open blocking or should-fix findings go to
// the coder as one message; with none left the review has passed.
func (m *Manager) closeRound(id string, data json.RawMessage) error {
	var d struct {
		Round int `json:"round"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &d); err != nil {
			return errors.New(`a verdict needs data like {"round": 1}`)
		}
	}
	l, unlock, err := m.begin(id)
	if err != nil {
		return err
	}
	defer unlock()
	target, _, _ := reviewers(l.ticket)
	r := l.state.Review
	switch {
	case r == nil || r.Status != domain.ReviewReviewing:
		return errors.New("no review round is waiting for a verdict")
	case d.Round != 0 && d.Round != r.Round:
		return fmt.Errorf("this is round %d, not %d", r.Round, d.Round)
	}
	records, _ := journal.Read(l.dir)
	findings, _ := roundRecords(records, target.Name, r.Rounds[len(r.Rounds)-1].At, time.Time{})
	verdict := ""
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind == VerdictKind && records[i].Service == domain.ReviewerService {
			verdict = records[i].Text
			break
		}
	}
	var open, nits []api.ReviewFinding
	closed := timeline.ReviewRoundClosedData{Round: r.Round, Verdict: timeline.Excerpt(verdict)}
	for _, f := range findings {
		switch f.Severity {
		case "nit":
			nits = append(nits, f)
			closed.Nit++
		case "should-fix":
			open = append(open, f)
			closed.ShouldFix++
		default:
			open = append(open, f)
			closed.Blocking++
		}
	}
	m.note(l.dir, timeline.ReviewRoundClosed, "agent:"+domain.ReviewerService, domain.ReviewerService, closed)
	if len(open) == 0 {
		reason := fmt.Sprintf("round %d: no blocking or should-fix findings", r.Round)
		m.setReview(l, func(r *state.Review) {
			r.Status, r.Reason, r.By = domain.ReviewPassed, reason, domain.ReviewerService
			r.Rounds[len(r.Rounds)-1].Verdict = verdict
		})
		m.note(l.dir, timeline.ReviewDecided, "agent:"+domain.ReviewerService, domain.ReviewerService, timeline.ReviewDecidedData{Outcome: "pass", Reason: reason})
		text := fmt.Sprintf("The review passed in round %d. Nothing more to do for it; wait to be moved on.", r.Round)
		if len(nits) > 0 {
			text += "\n\nNits, for your judgement only:" + listFindings(nits)
		}
		m.deliver(id, target.Name, text)
		return nil
	}
	m.setReview(l, func(r *state.Review) {
		r.Status = domain.ReviewFixing
		r.Rounds[len(r.Rounds)-1].Verdict = verdict
	})
	text := fmt.Sprintf("Review round %d of at most %d found %d to fix:%s", r.Round, l.ticket.Review.MaxRounds, len(open), listFindings(open))
	if len(nits) > 0 {
		text += "\n\nNits, for your judgement only:" + listFindings(nits)
	}
	text += "\n\nFix each, or disagree with your reason. Answer each with a review.reply record, commit, then leave review.ready."
	m.deliver(id, target.Name, text)
	return nil
}

func listFindings(fs []api.ReviewFinding) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "\n- %s [%s]", f.ID, f.Severity)
		if f.File != "" {
			fmt.Fprintf(&b, " %s", f.File)
			if f.Line > 0 {
				fmt.Fprintf(&b, ":%d", f.Line)
			}
		}
		b.WriteString(": " + f.What)
		if f.Scenario != "" {
			b.WriteString(" Scenario: " + f.Scenario)
		}
		if f.Fix != "" {
			b.WriteString(" Fix: " + f.Fix)
		}
	}
	return b.String()
}

// roundRecords are the reviewer's findings (the latest record per id) and the coder's
// replies (the latest per id) journaled in [from, until); a zero until means now.
func roundRecords(records []api.JournalRecord, coder string, from, until time.Time) ([]api.ReviewFinding, map[string]api.ReviewReply) {
	var findings []api.ReviewFinding
	index := map[string]int{}
	replies := map[string]api.ReviewReply{}
	for _, rec := range records {
		if rec.At.Before(from) || (!until.IsZero() && !rec.At.Before(until)) {
			continue
		}
		switch {
		case rec.Kind == ReviewResultKind && rec.Service == domain.ReviewerService:
			f, err := parseFinding(rec.Data)
			if err != nil {
				continue
			}
			if i, ok := index[f.ID]; ok {
				findings[i] = f
			} else {
				index[f.ID] = len(findings)
				findings = append(findings, f)
			}
		case rec.Kind == ReviewReplyKind && rec.Service == coder:
			if r, err := parseReply(rec.Data); err == nil {
				replies[r.ID] = r
			}
		}
	}
	return findings, replies
}

// ensureReviewer launches the reviewer's agent unless it is already running.
func (m *Manager) ensureReviewer(ctx context.Context, l loaded, reviewer domain.Service) error {
	if m.opts.Backend == nil || m.opts.Docker == nil {
		return errors.New("a reviewer runs in a container, which needs Docker and a terminal backend")
	}
	if pane := l.state.Services[reviewer.Name].Agent.PaneID; pane != "" {
		p, err := m.opts.Backend.Get(ctx, terminal.PaneID(pane))
		if err == nil && p.Alive {
			return nil
		}
		if err == nil {
			m.opts.Backend.Close(ctx, terminal.PaneID(pane))
		}
	}
	eff := policy.Resolve(m.opts.Global, l.ticket, reviewer, policy.Flags{})
	h, req, err := m.agentRequest(l, reviewer, eff)
	if err != nil {
		return err
	}
	if len(eff.AgentConfig) > 0 {
		if err := h.CheckTemplates(eff.AgentConfig, req.Secrets); err != nil {
			return err
		}
	}
	if err := m.opts.Backend.Ready(ctx); err != nil {
		return err
	}
	net, err := m.provisionNetworks(ctx, l, domain.SandboxContainer)
	if err != nil {
		return err
	}
	if net == nil {
		return errors.New("the ticket has no network for the reviewer's container")
	}
	proxyEnv, err := m.startProxy(ctx, l, net)
	if err != nil {
		return err
	}
	al, err := m.prepareContainerAgent(ctx, l, reviewer, eff, api.CheckoutInfo{Service: reviewer.Name, Kind: domain.CheckoutClone, Path: reviewerDir(l.dir)}, net, proxyEnv)
	if err != nil {
		return err
	}
	_, err = m.spawnAgent(ctx, l, reviewer, al)
	return err
}

// copyIgnored copies one path the repository ignores (installed dependencies) from the
// coder's checkout into the reviewer's copy, copy-on-write where the filesystem can. A
// link at the source or on the way to the destination is refused: it could pull in, or
// write to, anything on the host.
func copyIgnored(ctx context.Context, from, to, rel string) error {
	src, dst := filepath.Join(from, rel), filepath.Join(to, rel)
	info, err := os.Lstat(src)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil // nothing installed yet: the reviewer installs it if it needs to
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a link in the coder's checkout", rel)
	}
	parent := to
	for _, part := range strings.Split(filepath.Dir(filepath.Clean(rel)), string(filepath.Separator)) {
		if part == "." {
			break
		}
		parent = filepath.Join(parent, part)
		if pi, err := os.Lstat(parent); err == nil && pi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a link in the reviewer's copy", parent)
		} else if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(parent, 0o755); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	args := []string{"-R", "-P", src, dst}
	switch runtime.GOOS {
	case "darwin":
		if exec.CommandContext(ctx, "cp", append([]string{"-c"}, args...)...).Run() == nil {
			return nil
		}
		os.RemoveAll(dst)
	case "linux":
		args = []string{"-a", "--reflink=auto", src, dst}
	}
	if out, err := exec.CommandContext(ctx, "cp", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cp: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setReview changes the review loop in state.json and announces it.
func (m *Manager) setReview(l loaded, change func(*state.Review)) {
	var now state.Review
	err := m.opts.States.Update(l.statePath, func(s *state.State) error {
		if s.Review == nil {
			s.Review = &state.Review{}
		}
		change(s.Review)
		s.Review.At = m.opts.Clock.Now().UTC()
		now = *s.Review
		return nil
	})
	if err != nil {
		return
	}
	l.state.Review = &now
	m.publish(api.EventReviewUpdated, l.id, "", api.ReviewUpdatedData{Status: now.Status, Round: now.Round, Commit: now.Commit, Reason: now.Reason})
}

func agentsCost(st state.State) float64 {
	var total float64
	for _, s := range st.Services {
		if s.Agent.Cost != nil {
			total += s.Agent.Cost.USD
		}
	}
	return total
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// reviewTip is the reviewed service's branch tip in its checkout; false when it
// cannot be read (no checkout, a ticket without a review).
func reviewTip(ctx context.Context, t domain.Ticket, st state.State) (string, bool) {
	target, _, ok := reviewers(t)
	if !ok {
		return "", false
	}
	rec := st.Services[target.Name].Checkout
	if rec.Path == "" {
		return "", false
	}
	tip, err := git.BranchTip(ctx, rec.Path, target.Branch)
	return tip, err == nil
}

// reviewPin is why a passed review no longer lets the work go on: it passed one
// commit and the branch has moved. Empty when the review has not passed, when the
// pass pinned no commit (older tickets), or when the tip cannot be read.
func (m *Manager) reviewPin(ctx context.Context, id string, t domain.Ticket, st state.State) string {
	r := st.Review
	if r == nil || r.Status != domain.ReviewPassed || r.Commit == "" {
		return ""
	}
	tip, ok := reviewTip(ctx, t, st)
	if !ok || tip == r.Commit {
		return ""
	}
	return fmt.Sprintf("the review passed %s, but the branch is now at %s: journal review.ready for a round on the new work (or rig reviewer %s --start)", short(r.Commit), short(tip), id)
}

// ReviewerStart starts a round on a person's say-so; after a stop it starts a new loop.
func (m *Manager) ReviewerStart(ctx context.Context, p api.ReviewerParams) (api.ReviewerStatus, error) {
	if err := m.beginRound(p.Ticket, "human"); err != nil {
		return api.ReviewerStatus{}, err
	}
	m.runRound(ctx, p.Ticket)
	return m.ReviewerStatus(ctx, p)
}

// ReviewerDecide records a person's call: pass lets the ticket move on, stop ends the
// loop until a person starts it again.
func (m *Manager) ReviewerDecide(ctx context.Context, p api.ReviewerDecideParams) (api.ReviewerStatus, error) {
	outcome := map[string]domain.ReviewStatus{"pass": domain.ReviewPassed, "stop": domain.ReviewStopped}[p.Outcome]
	reason := strings.TrimSpace(p.Reason)
	switch {
	case outcome == "":
		return api.ReviewerStatus{}, invalid("outcome %q is not pass or stop", p.Outcome)
	case reason == "":
		return api.ReviewerStatus{}, invalid("a review decision needs a reason")
	}
	l, unlock, err := m.begin(p.Ticket)
	if err != nil {
		return api.ReviewerStatus{}, err
	}
	target, _, ok := reviewers(l.ticket)
	if !ok {
		unlock()
		return api.ReviewerStatus{}, invalid("ticket %q has no review in its ticket.yaml", p.Ticket)
	}
	// A person's pass is bound to the commit the branch is at now, as a reviewer's is
	// to the round's commit, so work committed after it is looked at again.
	tip, haveTip := reviewTip(ctx, l.ticket, l.state)
	m.setReview(l, func(r *state.Review) {
		r.Status, r.Reason, r.By = outcome, reason, "human"
		if outcome == domain.ReviewPassed && haveTip {
			r.Commit = tip
		}
	})
	m.note(l.dir, timeline.ReviewDecided, "human", "", timeline.ReviewDecidedData{Outcome: p.Outcome, Reason: reason})
	unlock()
	if outcome == domain.ReviewPassed {
		m.deliver(p.Ticket, target.Name, "A person passed the review: "+reason+". Wait to be moved on.")
	} else {
		m.deliver(p.Ticket, target.Name, "A person stopped the review loop: "+reason+". Wait for a person.")
	}
	return m.ReviewerStatus(ctx, api.ReviewerParams{Ticket: p.Ticket})
}

// ReviewerStatus is the loop in full: each round's findings and answers, and its cost.
func (m *Manager) ReviewerStatus(_ context.Context, p api.ReviewerParams) (api.ReviewerStatus, error) {
	l, err := m.load(p.Ticket)
	if err != nil {
		return api.ReviewerStatus{}, err
	}
	target, _, ok := reviewers(l.ticket)
	if !ok {
		return api.ReviewerStatus{}, invalid("ticket %q has no review in its ticket.yaml", p.Ticket)
	}
	res := api.ReviewerStatus{MaxRounds: l.ticket.Review.MaxRounds, BudgetUSD: l.ticket.Review.BudgetUSD}
	if c := l.state.Services[target.Name].Agent.Cost; c != nil {
		res.CoderUSD = c.USD
	}
	if c := l.state.Services[domain.ReviewerService].Agent.Cost; c != nil {
		res.ReviewerUSD = c.USD
	}
	r := l.state.Review
	if r == nil {
		return res, nil
	}
	res.Status, res.Round, res.Commit, res.Base, res.Reason, res.By = r.Status, r.Round, r.Commit, r.Base, r.Reason, r.By
	res.SpentUSD = agentsCost(l.state) - r.CostAtStart
	records, _ := journal.Read(l.dir)
	for i, round := range r.Rounds {
		var until time.Time
		if i+1 < len(r.Rounds) {
			until = r.Rounds[i+1].At
		}
		findings, replies := roundRecords(records, target.Name, round.At, until)
		for j := range findings {
			if rep, ok := replies[findings[j].ID]; ok {
				findings[j].Reply = &rep
			}
		}
		res.Rounds = append(res.Rounds, api.ReviewerRound{Round: round.Round, Commit: round.Commit, At: round.At, Verdict: round.Verdict, Findings: findings})
	}
	return res, nil
}

// removeReviewer drops the reviewer's copy and the main repository's review ref.
func (m *Manager) removeReviewer(ctx context.Context, l loaded) error {
	target, _, ok := reviewers(l.ticket)
	if !ok {
		return nil
	}
	if err := git.RemoveReview(ctx, target.Repo, l.id, reviewerDir(l.dir)); err != nil {
		return fmt.Errorf("remove the reviewer's copy: %w", err)
	}
	return nil
}
