package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/charlesrobsampson/rig/internal/domain"
)

type EventType string

const (
	EventTicketCreated     EventType = "ticket.created"
	EventTicketStarted     EventType = "ticket.started"
	EventTicketStopped     EventType = "ticket.stopped"
	EventServiceStatus     EventType = "service.status"
	EventPhaseChanged      EventType = "phase.changed"
	EventGateApproved      EventType = "gate.approved"
	EventAgentState        EventType = "agent.state"
	EventContractPending   EventType = "contract.pending"
	EventContractViolation EventType = "contract.violation"
	EventContractResolved  EventType = "contract.resolved"
	EventEgressDenied      EventType = "egress.denied"
	EventSummaryUpdated    EventType = "summary.updated"
	EventReconciled        EventType = "reconcile.done"
	EventError             EventType = "error"
	// EventSubscribed is the first item of every events.subscribe stream: the
	// subscription is live, so nothing published after it can be missed.
	EventSubscribed EventType = "subscribed"
)

// Event types are not validated on decode: a client must keep working when a newer
// daemon sends a type it has not heard of.
type Event struct {
	Type    EventType       `json:"type"`
	Ticket  string          `json:"ticket"`
	Service string          `json:"service"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data"`
}

func NewEvent(typ EventType, ticket, service string, at time.Time, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("encode %s event data: %w", typ, err)
	}
	return Event{Type: typ, Ticket: ticket, Service: service, At: at.UTC(), Data: raw}, nil
}

func (e Event) DecodeData(into any) error {
	if err := json.Unmarshal(e.Data, into); err != nil {
		return fmt.Errorf("decode %s event data: %w", e.Type, err)
	}
	return nil
}

type EmptyData struct{}

type ServiceStatusData struct {
	Status domain.ServiceStatus `json:"status"`
}

type PhaseChangedData struct {
	From   domain.Phase `json:"from"`
	To     domain.Phase `json:"to"`
	By     string       `json:"by"`
	Reason string       `json:"reason"`
}

type GateApprovedData struct {
	Gate    domain.Gate `json:"gate"`
	Service string      `json:"service"`
}

type AgentStateData struct {
	State    domain.AgentState `json:"state"`
	Previous domain.AgentState `json:"previous,omitempty"`
}

type ContractPendingData struct {
	Proposal string `json:"proposal"`
}

type ContractViolationData struct {
	Detail string `json:"detail"`
}

type ContractResolvedData struct {
	Proposal string `json:"proposal"`
	Outcome  string `json:"outcome"` // accepted or rejected
}

// ReconciledData says a reconcile pass finished and the set of drifting tickets may have
// changed; clients refresh their lists.
type ReconciledData struct {
	Drift int `json:"drift"`
	Fixed int `json:"fixed"`
}

// SummaryUpdatedData says a ticket has a new (or newly failed) language-model summary.
type SummaryUpdatedData struct {
	Backend string `json:"backend"`
	Error   string `json:"error,omitempty"`
}

type EgressDeniedData struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Reason string `json:"reason"`
}

type ErrorData struct {
	Message string `json:"message"`
}
