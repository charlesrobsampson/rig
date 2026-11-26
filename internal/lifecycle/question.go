package lifecycle

import (
	"context"
	"strings"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

// QuestionKind is the record kind an agent leaves to ask a person something.
const QuestionKind = "question"

// AskQuestion records an agent's question as the ticket's open one, so a person sees
// the ticket waiting. A newer question replaces an older one: the agent has moved on.
func (m *Manager) AskQuestion(id, service, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return invalid(`a question needs text: what the person must decide`)
	}
	l, unlock, err := m.begin(id)
	if err != nil {
		return err
	}
	defer unlock()
	if !hasService(l.ticket, service) {
		return invalid("ticket %q has no service %q", id, service)
	}
	at := m.opts.Clock.Now().UTC()
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		s.Question = &state.Question{Service: service, Text: text, At: at}
		return nil
	})
	if err != nil {
		return err
	}
	m.note(l.dir, timeline.QuestionAsked, "agent:"+service, service, timeline.QuestionAskedData{Text: timeline.Excerpt(text)})
	return nil
}

// AnswerQuestion is told of a message a person sent, and delivered, to a service. It
// closes the open question when that service is the one that asked; anything else
// leaves it open.
func (m *Manager) AnswerQuestion(id, service string) {
	l, unlock, err := m.begin(id)
	if err != nil {
		return
	}
	defer unlock()
	answered := false
	err = m.opts.States.Update(l.statePath, func(s *state.State) error {
		if s.Question != nil && s.Question.Service == service {
			s.Question = nil
			answered = true
		}
		return nil
	})
	if err == nil && answered {
		m.note(l.dir, timeline.QuestionAnswered, "human", service, struct{}{})
	}
}

// Rework sends a ticket back to implement because a person found something the review
// missed. The finding is the reason of the move, which the agents are told, and the
// miss is recorded as evidence for which reviewers to add.
func (m *Manager) Rework(ctx context.Context, p api.ReworkParams) (api.TransitionResult, error) {
	finding := strings.TrimSpace(p.Finding)
	if finding == "" {
		return api.TransitionResult{}, invalid("a rework needs a finding: what the review missed")
	}
	l, unlock, err := m.begin(p.ID)
	if err != nil {
		return api.TransitionResult{}, err
	}
	defer unlock()
	switch l.state.Phase {
	case domain.PhaseTest, domain.PhaseReview, domain.PhasePush:
	default:
		return api.TransitionResult{}, invalid("ticket %q is in %s: rework sends work from test, review or push back to implement (use rig phase to move it otherwise)", p.ID, l.state.Phase)
	}
	service := ""
	if l.ticket.Review != nil {
		service = l.ticket.Review.Of
	} else if len(l.ticket.Services) == 1 {
		service = l.ticket.Services[0].Name
	}
	res, err := m.moveLocked(l, p.ID, "human", domain.PhaseImplement, finding, service)
	if err != nil {
		return res, err
	}
	m.note(l.dir, timeline.ReviewMissed, "human", service, timeline.ReviewMissedData{Finding: timeline.Excerpt(finding), Lens: strings.TrimSpace(p.Lens)})
	return res, nil
}
