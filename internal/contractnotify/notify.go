// Package contractnotify tells the agents about contract changes: a new proposal goes
// to every service but the one that wrote it, an accepted proposal goes to all of
// them, and a rejection goes back to the proposer with the reason. Messages are typed
// into the agents' panes through the same injector as `rig msg`, so they obey its
// rules (never into an agent waiting on a prompt, logged in messages.jsonl).
package contractnotify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/contract"
)

// Sender types a message into a service's agent; *inject.Injector is one.
type Sender interface {
	Send(ctx context.Context, p api.AgentMessageParams, from string) (api.AgentMessageResult, error)
}

type Notifier struct {
	Dir    string // the tickets directory
	Sender Sender
	Logger *slog.Logger
	// Timeout bounds one delivery (default 2 minutes; an agent may be busy starting).
	Timeout time.Duration

	wg sync.WaitGroup
}

func (n *Notifier) logger() *slog.Logger {
	if n.Logger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return n.Logger
}

// Wait blocks until every delivery started so far has finished (used by tests and at
// shutdown).
func (n *Notifier) Wait() { n.wg.Wait() }

// NewProposal tells every service except the proposer that a change is waiting for the
// human's decision.
func (n *Notifier) NewProposal(ticket string, p contract.Proposal) {
	text := fmt.Sprintf("Contract change %s proposed by %s is awaiting the human's decision: %s. Do not rely on it until it is accepted. You will be told the outcome.", p.ID, p.From, oneLine(p.What))
	for _, svc := range n.services(ticket) {
		if svc != p.From {
			n.send(ticket, svc, text)
		}
	}
}

// Resolved tells the agents how a proposal ended: everyone hears about an acceptance;
// only the proposer is told of a rejection, with the reason.
func (n *Notifier) Resolved(ticket string, p contract.Proposal, outcome, reason string) {
	if outcome == "accepted" {
		text := fmt.Sprintf("Contract change %s from %s was accepted and is now part of the agreed section of contract.md; re-read it.", p.ID, p.From)
		for _, svc := range n.services(ticket) {
			n.send(ticket, svc, text)
		}
		return
	}
	text := fmt.Sprintf("Your contract proposal %s was rejected", p.ID)
	if reason != "" {
		text += ": " + oneLine(reason)
	}
	text += ". The agreed section of contract.md is unchanged; work within it or propose something else."
	n.send(ticket, p.From, text)
}

func (n *Notifier) services(ticket string) []string {
	t, err := config.LoadTicket(filepath.Join(n.Dir, ticket, "ticket.yaml"), config.TicketOptions{RepoCheck: func(string) error { return nil }})
	if err != nil {
		n.logger().Warn("contract notice: read ticket", "ticket", ticket, "err", err)
		return nil
	}
	var out []string
	for _, s := range t.Services {
		out = append(out, s.Name)
	}
	return out
}

func (n *Notifier) send(ticket, service, text string) {
	timeout := n.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if _, err := n.Sender.Send(ctx, api.AgentMessageParams{Ticket: ticket, Service: service, Text: text}, "rig"); err != nil {
			n.logger().Warn("contract notice not delivered", "ticket", ticket, "service", service, "err", err)
		}
	}()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
