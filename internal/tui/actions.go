package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// Actions is what the TUI can do to a ticket. Each method is the same daemon call the
// CLI makes, so a key press and a command line do exactly the same thing.
type Actions interface {
	Approve(ctx context.Context, ticket string, gate domain.Gate) error
	MovePhase(ctx context.Context, ticket string, to domain.Phase) error
	Push(ctx context.Context, ticket string) (api.PushRunResult, error)
	Stop(ctx context.Context, ticket string, force bool) (api.TicketStopResult, error)
	Message(ctx context.Context, ticket, service, text string) error
	Proposals(ctx context.Context, ticket string) ([]api.ContractProposal, error)
	Resolve(ctx context.Context, ticket, proposal string, accept bool, reason string) error
	// AttachCommand is what shows a service's agent pane.
	AttachCommand(ctx context.Context, ticket, service string) ([]string, error)
}

// modal is a question the user must answer before anything else happens: a yes/no
// confirmation, or a line of text.
type modal struct {
	prompt string
	// confirm modals take y or n; prompt modals edit a line of text.
	confirm bool
	input   []rune
	// submit runs when the user answers yes, or presses enter in a prompt. It gets the
	// text typed (empty for a confirmation).
	submit func(m Model, text string) (Model, tea.Cmd)
}

// Messages that carry an action's outcome back into the model.
type (
	// doneMsg is an action finished: text is the status line, err what went wrong. A
	// success refreshes the fleet and the open tab.
	doneMsg struct {
		text string
		err  error
		// force, set with a blocked stop, offers to repeat the stop discarding work.
		force *forceStop
	}
	forceStop struct {
		ticket  string
		details []string
	}
	// proposalsMsg is the open proposals of a ticket's contract, for accept or reject.
	proposalsMsg struct {
		ticket string
		accept bool
		open   []api.ContractProposal
		err    error
	}
	// attachMsg is the command that shows an agent, ready to run with the TUI suspended.
	attachMsg struct {
		ticket, service string
		command         []string
		err             error
	}
	// execDoneMsg says the attached program exited and the TUI is back.
	execDoneMsg struct{ err error }
)

// selected is the ticket actions apply to: the open one, else the highlighted row.
func (m Model) selected() (api.TicketSummary, bool) {
	if m.det != nil {
		for _, t := range m.tickets {
			if t.ID == m.det.ticket {
				return t, true
			}
		}
		return api.TicketSummary{ID: m.det.ticket}, true
	}
	return m.dash.current()
}

func (m Model) fail(format string, args ...any) Model {
	m.status, m.statusErr = fmt.Sprintf(format, args...), true
	return m
}

func (m Model) ok(format string, args ...any) Model {
	m.status, m.statusErr = fmt.Sprintf(format, args...), false
	return m
}

// action handles the action keys; handled is false for any other key.
func (m Model) action(k string) (next Model, cmd tea.Cmd, handled bool) {
	switch k {
	case "a", "v", "p", "x", "t", "m", "c", "C":
	default:
		return m, nil, false
	}
	t, ok := m.selected()
	if !ok {
		return m.fail("no ticket selected"), nil, true
	}
	if m.opts.Source == nil {
		return m.fail("no daemon connection configured"), nil, true
	}
	switch k {
	case "a":
		next, cmd = m.approve(t)
	case "v":
		next, cmd = m.review(t)
	case "p":
		next, cmd = m.pushTicket(t)
	case "x":
		next, cmd = m.stopTicket(t)
	case "t":
		next, cmd = m.attach(t)
	case "m":
		next, cmd = m.message(t)
	case "c":
		next, cmd = m.resolve(t, true)
	case "C":
		next, cmd = m.resolve(t, false)
	}
	return next, cmd, true
}

func (m Model) confirm(prompt string, submit func(Model, string) (Model, tea.Cmd)) Model {
	m.modal = &modal{prompt: prompt + " [y/N]", confirm: true, submit: submit}
	return m
}

func (m Model) ask(prompt string, submit func(Model, string) (Model, tea.Cmd)) Model {
	m.modal = &modal{prompt: prompt, submit: submit}
	return m
}

// run turns an action into a command whose result is a doneMsg.
func (m Model) run(f func(ctx context.Context, src Source) doneMsg) tea.Cmd {
	src := m.opts.Source
	return func() tea.Msg { return f(context.Background(), src) }
}

func (m Model) approve(t api.TicketSummary) (Model, tea.Cmd) {
	if t.PendingGate == "" {
		return m.fail("%s: nothing waits for your approval", t.ID), nil
	}
	gate := t.PendingGate
	return m.confirm(fmt.Sprintf("Approve %s for %s?", gate, t.ID), func(m Model, _ string) (Model, tea.Cmd) {
		return m, m.run(func(ctx context.Context, src Source) doneMsg {
			if err := src.Approve(ctx, t.ID, gate); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: fmt.Sprintf("%s approved for %s", gate, t.ID)}
		})
	}), nil
}

func (m Model) review(t api.TicketSummary) (Model, tea.Cmd) {
	return m.confirm(fmt.Sprintf("Move %s to the review phase?", t.ID), func(m Model, _ string) (Model, tea.Cmd) {
		return m, m.run(func(ctx context.Context, src Source) doneMsg {
			if err := src.MovePhase(ctx, t.ID, domain.PhaseReview); err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: t.ID + " is in review"}
		})
	}), nil
}

func (m Model) pushTicket(t api.TicketSummary) (Model, tea.Cmd) {
	return m.confirm(fmt.Sprintf("Push %s? Its branches go to their remotes as each service's push mode says.", t.ID), func(m Model, _ string) (Model, tea.Cmd) {
		return m, m.run(func(ctx context.Context, src Source) doneMsg {
			res, err := src.Push(ctx, t.ID)
			if err != nil {
				return doneMsg{err: err}
			}
			return doneMsg{text: pushSummary(t.ID, res)}
		})
	}), nil
}

// pushSummary is one status line for a push: what went through and what did not.
func pushSummary(ticket string, res api.PushRunResult) string {
	var failed []string
	pushed := 0
	for _, s := range res.Services {
		switch {
		case s.Error != "":
			failed = append(failed, s.Service+": "+s.Error)
		case s.Pushed || s.Skipped:
			pushed++
		}
	}
	switch {
	case len(failed) > 0:
		return fmt.Sprintf("push of %s: %d of %d services pushed; failed — %s", ticket, pushed, len(res.Services), strings.Join(failed, "; "))
	case res.Done:
		return fmt.Sprintf("%s pushed and done", ticket)
	}
	return fmt.Sprintf("push of %s: %d of %d services done", ticket, pushed, len(res.Services))
}

func (m Model) stopTicket(t api.TicketSummary) (Model, tea.Cmd) {
	return m.confirm(fmt.Sprintf("Stop %s? Work is imported first and branches are kept; checkouts are removed.", t.ID), func(m Model, _ string) (Model, tea.Cmd) {
		return m, m.stopCmd(t.ID, false)
	}), nil
}

func (m Model) stopCmd(ticket string, force bool) tea.Cmd {
	return m.run(func(ctx context.Context, src Source) doneMsg {
		_, err := src.Stop(ctx, ticket, force)
		var apiErr *api.Error
		switch {
		case err == nil:
			return doneMsg{text: ticket + " stopped"}
		case !force && errors.As(err, &apiErr) && apiErr.Code == api.CodeBlocked:
			// work that deleting would lose: say what, and offer to discard it
			return doneMsg{err: err, force: &forceStop{ticket: ticket, details: apiErr.Details}}
		}
		return doneMsg{err: err}
	})
}

// service picks which service an action is for: the only one, or the one the user
// names. use runs with the chosen name.
func (m Model) service(t api.TicketSummary, use func(Model, string) (Model, tea.Cmd)) (Model, tea.Cmd) {
	switch len(t.Services) {
	case 0:
		return m.fail("%s has no services", t.ID), nil
	case 1:
		return use(m, t.Services[0].Name)
	}
	names := make([]string, len(t.Services))
	for i, s := range t.Services {
		names[i] = s.Name
	}
	return m.ask(fmt.Sprintf("Which service? (%s; enter = %s)", strings.Join(names, ", "), names[0]), func(m Model, text string) (Model, tea.Cmd) {
		text = strings.TrimSpace(text)
		switch {
		case text == "":
			return use(m, names[0])
		case slices.Contains(names, text):
			return use(m, text)
		}
		return m.fail("%s has no service %q (it has %s)", t.ID, text, strings.Join(names, ", ")), nil
	}), nil
}

func (m Model) message(t api.TicketSummary) (Model, tea.Cmd) {
	return m.service(t, func(m Model, svc string) (Model, tea.Cmd) {
		return m.ask(fmt.Sprintf("Message to %s/%s:", t.ID, svc), func(m Model, text string) (Model, tea.Cmd) {
			if strings.TrimSpace(text) == "" {
				return m.fail("nothing to send"), nil
			}
			return m, m.run(func(ctx context.Context, src Source) doneMsg {
				if err := src.Message(ctx, t.ID, svc, text); err != nil {
					return doneMsg{err: err}
				}
				return doneMsg{text: fmt.Sprintf("sent to %s/%s", t.ID, svc)}
			})
		}), nil
	})
}

func (m Model) attach(t api.TicketSummary) (Model, tea.Cmd) {
	return m.service(t, func(m Model, svc string) (Model, tea.Cmd) {
		src := m.opts.Source
		return m, func() tea.Msg {
			cmd, err := src.AttachCommand(context.Background(), t.ID, svc)
			return attachMsg{ticket: t.ID, service: svc, command: cmd, err: err}
		}
	})
}

// resolve starts accepting or rejecting a contract proposal: it asks the daemon for the
// open ones, and proposalsMsg carries on from there.
func (m Model) resolve(t api.TicketSummary, accept bool) (Model, tea.Cmd) {
	src := m.opts.Source
	return m, func() tea.Msg {
		all, err := src.Proposals(context.Background(), t.ID)
		var open []api.ContractProposal
		for _, p := range all {
			if p.Status == "open" {
				open = append(open, p)
			}
		}
		return proposalsMsg{ticket: t.ID, accept: accept, open: open, err: err}
	}
}

func (m Model) onProposals(msg proposalsMsg) (Model, tea.Cmd) {
	switch {
	case msg.err != nil && errors.Is(msg.err, ErrMissing):
		return m.fail("%s has no contract (only tickets with several services do)", msg.ticket), nil
	case msg.err != nil:
		return m.fail("%v", msg.err), nil
	case len(msg.open) == 0:
		return m.ok("%s has no open contract proposals", msg.ticket), nil
	case len(msg.open) == 1:
		return m.decide(msg.ticket, msg.accept, msg.open[0])
	}
	ids := make([]string, len(msg.open))
	for i, p := range msg.open {
		ids[i] = p.ID
	}
	return m.ask(fmt.Sprintf("Which proposal? (%s; enter = %s)", strings.Join(ids, ", "), ids[0]), func(m Model, text string) (Model, tea.Cmd) {
		text = strings.TrimSpace(text)
		for _, p := range msg.open {
			if text == "" && p.ID == ids[0] || text == p.ID {
				return m.decide(msg.ticket, msg.accept, p)
			}
		}
		return m.fail("%q is not one of the open proposals (%s)", text, strings.Join(ids, ", ")), nil
	}), nil
}

// decide accepts a proposal after a confirmation, or rejects it with the reason the
// user types (a reason is required, and typing it is the confirmation).
func (m Model) decide(ticket string, accept bool, p api.ContractProposal) (Model, tea.Cmd) {
	if accept {
		return m.confirm(fmt.Sprintf("Accept %s from %s into the contract: %s?", p.ID, p.From, firstLine(p.What)), func(m Model, _ string) (Model, tea.Cmd) {
			return m, m.resolveCmd(ticket, p.ID, true, "")
		}), nil
	}
	return m.ask(fmt.Sprintf("Reject %s from %s — reason:", p.ID, p.From), func(m Model, reason string) (Model, tea.Cmd) {
		if strings.TrimSpace(reason) == "" {
			return m.fail("a rejection needs a reason; nothing changed"), nil
		}
		return m, m.resolveCmd(ticket, p.ID, false, reason)
	}), nil
}

func (m Model) resolveCmd(ticket, id string, accept bool, reason string) tea.Cmd {
	return m.run(func(ctx context.Context, src Source) doneMsg {
		if err := src.Resolve(ctx, ticket, id, accept, reason); err != nil {
			return doneMsg{err: err}
		}
		verb := "rejected"
		if accept {
			verb = "accepted"
		}
		return doneMsg{text: fmt.Sprintf("%s %s", id, verb)}
	})
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

// onDone shows an action's outcome. A success refreshes what is on screen; a failure
// changes nothing but the status line, except that a stop refused for unsaved work
// offers to discard it.
func (m Model) onDone(msg doneMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		if msg.force != nil {
			f := msg.force
			detail := strings.Join(f.details, "; ")
			if detail == "" {
				detail = msg.err.Error()
			}
			return m.confirm(fmt.Sprintf("%s cannot stop without losing work (%s). Force it and discard that work?", f.ticket, detail), func(m Model, _ string) (Model, tea.Cmd) {
				return m, m.stopCmd(f.ticket, true)
			}), nil
		}
		return m.fail("%s", errText(msg.err)), nil
	}
	m = m.ok("%s", msg.text)
	cmds := []tea.Cmd{m.load()}
	if m.det != nil {
		cmds = append(cmds, m.loadTab(m.det.tab))
	}
	return m, tea.Batch(cmds...)
}

// errText is an error on one line, with the reasons of a refused request.
func errText(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if len(apiErr.Details) > 0 {
			return apiErr.Message + " — " + strings.Join(apiErr.Details, "; ")
		}
		return apiErr.Message
	}
	return strings.ReplaceAll(err.Error(), "\n", " ")
}

// onAttach hands the terminal to the agent's pane and comes back when it exits.
func (m Model) onAttach(msg attachMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m.fail("%s", errText(msg.err)), nil
	}
	if len(msg.command) == 0 {
		return m.fail("the daemon returned no attach command for %s/%s", msg.ticket, msg.service), nil
	}
	run := m.opts.Exec
	if run == nil {
		run = func(argv []string, done func(error) tea.Msg) tea.Cmd {
			return tea.ExecProcess(exec.Command(argv[0], argv[1:]...), func(err error) tea.Msg { return done(err) })
		}
	}
	return m.ok("attached to %s/%s", msg.ticket, msg.service), run(msg.command, func(err error) tea.Msg { return execDoneMsg{err: err} })
}

func (m Model) onExecDone(msg execDoneMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m = m.fail("attach ended with an error: %v", msg.err)
	} else {
		m = m.ok("back from the agent")
	}
	cmds := []tea.Cmd{m.load()}
	if m.det != nil {
		cmds = append(cmds, m.loadTab(m.det.tab))
	}
	return m, tea.Batch(cmds...)
}

// modalKey feeds a key to the open question.
func (m Model) modalKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	md := m.modal
	if md.confirm {
		m.modal = nil
		if k := msg.String(); k == "y" || k == "Y" {
			return md.submit(m, "")
		}
		return m.ok("cancelled"), nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.modal = nil
		return m.ok("cancelled"), nil
	case tea.KeyEnter:
		m.modal = nil
		return md.submit(m, string(md.input))
	case tea.KeyBackspace:
		if n := len(md.input); n > 0 {
			md.input = md.input[:n-1]
		}
	case tea.KeySpace:
		md.input = append(md.input, ' ')
	case tea.KeyRunes:
		md.input = append(md.input, msg.Runes...)
	}
	return m, nil
}
