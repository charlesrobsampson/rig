package api

import (
	"time"

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
//
// DryRun provisions nothing and launches nothing: it only reports, per service, the
// agent launch that a start would use (secret values are never included).
type TicketStartParams struct {
	ID      string         `json:"id"`
	Sandbox domain.Sandbox `json:"sandbox,omitempty"`
	DryRun  bool           `json:"dry_run,omitempty"`
}

type TicketStartResult struct {
	Summary   TicketSummary  `json:"summary"`
	Checkouts []CheckoutInfo `json:"checkouts"`
	// Launches is set only for a dry run.
	Launches []LaunchInfo `json:"launches,omitempty"`
}

// LaunchInfo is what would start a service's agent. Environment values that come from
// secrets are shown as "<secret NAME>".
type LaunchInfo struct {
	Service string            `json:"service"`
	Harness string            `json:"harness"`
	Dir     string            `json:"dir"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
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

// TicketStopParams tears a ticket's checkouts down. Force discards uncommitted and
// unimportable work instead of refusing; KeepCheckouts imports and marks the services
// stopped but leaves the directories alone.
type TicketStopParams struct {
	ID            string `json:"id"`
	Force         bool   `json:"force,omitempty"`
	KeepCheckouts bool   `json:"keep_checkouts,omitempty"`
}

type TicketStopResult struct {
	Summary  TicketSummary `json:"summary"`
	Services []StopInfo    `json:"services"`
}

// StopInfo says what happened to one service's checkout.
type StopInfo struct {
	Service string `json:"service"`
	// Imported: a clone's branch was fetched into the main repository.
	Imported bool `json:"imported"`
	Removed  bool `json:"removed"`
}

type GateListParams struct {
	ID string `json:"id"`
}

// GateListResult lists every gate any service of the ticket requires, with whether it
// has been approved. Approving a gate returns the same list, updated.
type GateListResult struct {
	Phase domain.Phase `json:"phase"`
	Gates []GateStatus `json:"gates"`
}

// GateStatus is one gate one service must pass.
type GateStatus struct {
	Gate     domain.Gate  `json:"gate"`
	From     domain.Phase `json:"from"`
	To       domain.Phase `json:"to"`
	Service  string       `json:"service"`
	Approved bool         `json:"approved"`
	By       string       `json:"by,omitempty"`
	At       *time.Time   `json:"at,omitempty"`
	// Current: this gate guards the move out of the ticket's current phase.
	Current bool `json:"current"`
}

// GateApproveParams records a human approval. An empty Service approves the gate for
// every service that requires it.
type GateApproveParams struct {
	ID      string      `json:"id"`
	Gate    domain.Gate `json:"gate"`
	Service string      `json:"service,omitempty"`
}

// PhaseSetParams is a human moving a ticket to another phase. Moving backward needs a
// Reason.
type PhaseSetParams struct {
	ID     string       `json:"id"`
	To     domain.Phase `json:"to"`
	Reason string       `json:"reason,omitempty"`
}

// TransitionRequestParams is an agent asking for the same move. Service names the
// service whose agent is asking. The rules are exactly the human's.
type TransitionRequestParams struct {
	ID      string       `json:"id"`
	Service string       `json:"service"`
	To      domain.Phase `json:"to"`
	Reason  string       `json:"reason,omitempty"`
}

// TransitionResult describes the move that was made.
type TransitionResult struct {
	Summary TicketSummary `json:"summary"`
	From    domain.Phase  `json:"from"`
	To      domain.Phase  `json:"to"`
	By      string        `json:"by"`
	Reason  string        `json:"reason"`
	At      time.Time     `json:"at"`
}

// AgentEventParams is what an agent's hook reports: the agent of ticket/service is now
// in State (ready, working, blocked, idle or exited). "ready" means the agent's
// interface is up and can take typed input; it counts as idle.
type AgentEventParams struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service"`
	State   string `json:"state"`
}
