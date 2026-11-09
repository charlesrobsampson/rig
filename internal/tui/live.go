package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// Subscriber is the daemon's event stream. Subscribe returns once the subscription is
// live (nothing published after that can be missed) and the channel then carries every
// event; the channel closes when the subscription ends, however it ends.
type Subscriber interface {
	Subscribe(ctx context.Context) (<-chan api.Event, error)
}

// Messages from the event pump into the model.
type (
	// subscribedMsg: the subscription is live. The fleet may have changed since it was
	// last read, so the model reads it afresh.
	subscribedMsg struct{}
	// eventsMsg is a burst of events delivered together, so a flood costs one update.
	eventsMsg struct{ events []api.Event }
	// droppedMsg: the subscription ended or could not be made (err says why, nil when
	// it just ended). The model refreshes; the pump is already resubscribing.
	droppedMsg struct{ err error }
)

// maxBatch bounds how many events are delivered in one message.
const maxBatch = 256

// Pump keeps the subscription alive and feeds the model through send (a tea.Program's
// Send): subscribedMsg when it is live, eventsMsg per burst, droppedMsg when it is lost.
// It resubscribes at once after a drop, and waits retry between failed attempts, until
// ctx ends.
func Pump(ctx context.Context, src Subscriber, send func(tea.Msg), retry time.Duration) {
	if retry <= 0 {
		retry = 2 * time.Second
	}
	wait := func() bool {
		t := time.NewTimer(retry)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for ctx.Err() == nil {
		started := time.Now()
		ch, err := src.Subscribe(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			send(droppedMsg{err: err})
			if !wait() {
				return
			}
			continue
		}
		send(subscribedMsg{})
	stream:
		for {
			var batch []api.Event
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					break stream
				}
				batch = append(batch, ev)
			}
			for len(batch) < maxBatch { // whatever is already waiting goes in the same message
				select {
				case ev, ok := <-ch:
					if !ok {
						send(eventsMsg{batch})
						break stream
					}
					batch = append(batch, ev)
					continue
				default:
				}
				break
			}
			send(eventsMsg{batch})
		}
		if ctx.Err() != nil {
			return
		}
		send(droppedMsg{})
		if time.Since(started) < retry && !wait() { // a stream that dies at once must not spin
			return
		}
	}
}

// applyEvents folds events into the fleet. agent.state and phase.changed update the
// row in place; anything else that can change a row's summary asks for a fresh read.
// It reports whether the fleet must be read again and whether the open ticket changed.
func (m *Model) applyEvents(events []api.Event) (refresh, openChanged bool) {
	for _, ev := range events {
		touchesOpen := m.det != nil && ev.Ticket == m.det.ticket
		switch ev.Type {
		case api.EventAgentState:
			var d api.AgentStateData
			if ev.DecodeData(&d) != nil {
				refresh = true
				continue
			}
			if !m.setAgentState(ev.Ticket, ev.Service, d.State) {
				refresh = true // a ticket or service we have not seen
			}
		case api.EventAgentCost:
			var d api.AgentCostData
			if ev.DecodeData(&d) != nil || !m.setCost(ev.Ticket, ev.Service, d.USD) {
				refresh = true
			}
		case api.EventPhaseChanged:
			var d api.PhaseChangedData
			if ev.DecodeData(&d) != nil || !m.setPhase(ev.Ticket, d.To) {
				refresh = true
				continue
			}
			refresh = true // the pending gate follows the phase, and only the daemon knows it
			openChanged = openChanged || touchesOpen
		case api.EventTicketCreated, api.EventTicketStarted, api.EventTicketStopped, api.EventServiceStatus,
			api.EventGateApproved, api.EventContractPending, api.EventContractResolved, api.EventContractViolation, api.EventReconciled, api.EventSummaryUpdated:
			refresh = true
			openChanged = openChanged || touchesOpen
		}
	}
	return refresh, openChanged
}

func (m *Model) setAgentState(ticket, service string, state domain.AgentState) bool {
	for i := range m.tickets {
		if m.tickets[i].ID != ticket {
			continue
		}
		for j := range m.tickets[i].Services {
			if m.tickets[i].Services[j].Name == service {
				m.tickets[i].Services[j].AgentState = state
				return true
			}
		}
	}
	return false
}

func (m *Model) setCost(ticket, service string, usd float64) bool {
	for i := range m.tickets {
		if m.tickets[i].ID != ticket {
			continue
		}
		for j := range m.tickets[i].Services {
			if m.tickets[i].Services[j].Name == service {
				m.tickets[i].Services[j].CostUSD = &usd
				return true
			}
		}
	}
	return false
}

func (m *Model) setPhase(ticket string, to domain.Phase) bool {
	for i := range m.tickets {
		if m.tickets[i].ID == ticket {
			m.tickets[i].Phase = to
			return true
		}
	}
	return false
}
