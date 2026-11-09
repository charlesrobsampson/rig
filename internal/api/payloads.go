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

// TicketNewParams creates a ticket. With From ("type:ref", e.g. "jira:PROJ-412") the
// ticket is seeded from that source; ID, Title and Description, when set, override what
// the source says. Without From, ID and Title come from the caller (a title alone is
// enough: the id is derived from it).
type TicketNewParams struct {
	ID          string        `json:"id,omitempty"`
	Title       string        `json:"title,omitempty"`
	Description string        `json:"description,omitempty"`
	From        string        `json:"from,omitempty"`
	Services    []ServiceSpec `json:"services"`
}

// ServiceSpec names a service and where its code lives. Repo must be an absolute
// path; an empty Branch becomes <branch prefix><ticket id>/<service name>.
type ServiceSpec struct {
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
}

type TicketNewResult struct {
	Summary TicketSummary `json:"summary"`
	// Path is the new ticket directory.
	Path string `json:"path"`
}

// TicketStartParams provisions a ticket's checkouts. Sandbox, when set, overrides the
// sandbox the ticket and global config would choose for every service.
type TicketStartParams struct {
	ID      string         `json:"id"`
	Sandbox domain.Sandbox `json:"sandbox,omitempty"`
}

type TicketStartResult struct {
	Summary   TicketSummary  `json:"summary"`
	Checkouts []CheckoutInfo `json:"checkouts"`
}

// CheckoutInfo says where a service's code lives. Created is false when the checkout
// already existed (starting is idempotent).
type CheckoutInfo struct {
	Service string              `json:"service"`
	Kind    domain.CheckoutKind `json:"kind"`
	Path    string              `json:"path"`
	Branch  string              `json:"branch"`
	Created bool                `json:"created"`
}
