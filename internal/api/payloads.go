package api

import (
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
)

type StatusResult struct {
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

type TicketListResult struct {
	Tickets []TicketSummary `json:"tickets"`
}

// TicketSummary is one row of `rig list`. A ticket whose files cannot be read still
// appears, with Error set and the unreadable parts left empty.
type TicketSummary struct {
	ID              string           `json:"id"`
	Title           string           `json:"title"`
	Phase           domain.Phase     `json:"phase,omitempty"`
	Services        []ServiceSummary `json:"services"`
	ContractPending bool             `json:"contract_pending"`
	Error           string           `json:"error,omitempty"`
}

type ServiceSummary struct {
	Name       string               `json:"name"`
	Status     domain.ServiceStatus `json:"status,omitempty"`
	AgentState domain.AgentState    `json:"agent_state,omitempty"`
	HostPort   int                  `json:"host_port,omitempty"`
}

type TicketGetParams struct {
	ID string `json:"id"`
}

// TicketGetResult carries whatever could be read: Ticket and State are nil when
// ticket.yaml or state.json is missing or broken (Summary.Error says why).
type TicketGetResult struct {
	Summary TicketSummary  `json:"summary"`
	Ticket  *domain.Ticket `json:"ticket,omitempty"`
	State   *state.State   `json:"state,omitempty"`
}
