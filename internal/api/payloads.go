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
	// Agents says, per service, whether an agent was started (never for a dry run).
	Agents []AgentInfo `json:"agents,omitempty"`
	// Network is the ticket's Docker network; nil when the ticket needs no Docker.
	Network *NetworkInfo `json:"network,omitempty"`
	// Containers says, per service that runs in Docker, how its container went.
	Containers []ServiceContainerInfo `json:"containers,omitempty"`
	// Warnings are things the user should know about this start (an agent on the host).
	Warnings []string `json:"warnings,omitempty"`
	// Dependencies says how each sidecar (a database, a queue) went; Service holds its name.
	Dependencies []ServiceContainerInfo `json:"dependencies,omitempty"`
}

// ServiceContainerInfo is the outcome of running one service's container. Running is
// false with Note when nothing needed doing (already running) and with Error when it
// failed (build, start, or health); Error then ends with the container's last log lines.
type ServiceContainerInfo struct {
	Service  string `json:"service"`
	Name     string `json:"name,omitempty"`
	ID       string `json:"id,omitempty"`
	HostPort int    `json:"host_port,omitempty"`
	URL      string `json:"url,omitempty"`
	Running  bool   `json:"running"`
	Note     string `json:"note,omitempty"`
	Error    string `json:"error,omitempty"`
}

// NetworkInfo names a ticket's Docker networks. Egress is set only in internal mode.
type NetworkInfo struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Internal bool   `json:"internal"`
	Egress   string `json:"egress,omitempty"`
}

// AgentInfo is the outcome of starting one service's agent. Launched is false with
// Note when none was needed or allowed (already running, or a sandbox that cannot run
// agents yet) and with Error when starting it failed.
type AgentInfo struct {
	Service string `json:"service"`
	PaneID  string `json:"pane_id,omitempty"`
	// Container is the agent's container, when it runs in one.
	Container string `json:"container,omitempty"`
	Launched  bool   `json:"launched"`
	Note      string `json:"note,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AgentAttachParams names the agent to look at. Service may be empty when the ticket
// has exactly one.
type AgentAttachParams struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service,omitempty"`
}

// AgentAttachResult is the command a human runs to see the agent's pane.
type AgentAttachResult struct {
	Service string   `json:"service"`
	Command []string `json:"command"`
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
	// Resources lists the Docker containers and networks this stop removed
	// ("container rig-demo-svc1", "network rig-demo").
	Resources []string `json:"resources,omitempty"`
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

// AgentMessageParams types Text into a service's running agent, as if a human had
// typed it and pressed Enter (docs/spikes/03-tmux.md §3). Rig prefixes it with "[rig]".
type AgentMessageParams struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service"`
	Text    string `json:"text"`
}

// AgentMessageResult reports what reached the agent: Sent is the exact text typed
// (prefix and escaping included).
type AgentMessageResult struct {
	Service string `json:"service"`
	Sent    string `json:"sent"`
}

// LogsParams asks for container logs of a ticket. Service names a service, a
// dependency, or "proxy"; empty means all of them. Tail limits each container to its
// last N lines (0 = all); Follow keeps streaming new lines until the client leaves.
type LogsParams struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service,omitempty"`
	Tail    int    `json:"tail,omitempty"`
	Follow  bool   `json:"follow,omitempty"`
}

// LogLine is one line of one container's output; Service is its name as in ticket.yaml
// ("web", "db", or "proxy").
type LogLine struct {
	Service string `json:"service"`
	Line    string `json:"line"`
}

// ReviewDiffParams asks for what a ticket's services changed. Service may be empty for all.
type ReviewDiffParams struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service,omitempty"`
}

type ReviewDiffResult struct {
	Services []ServiceDiff `json:"services"`
}

// ServiceDiff is one service's change since its branch left Base, committed or not.
// Patch is a unified diff; a service without a checkout has Note instead, and one whose
// diff could not be built has Error.
type ServiceDiff struct {
	Service   string       `json:"service"`
	Branch    string       `json:"branch"`
	Base      string       `json:"base"`
	MergeBase string       `json:"merge_base,omitempty"`
	Files     []FileChange `json:"files"`
	Patch     string       `json:"patch"`
	Note      string       `json:"note,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// FileChange is one file in a ServiceDiff; Untracked files are new and not yet added.
type FileChange struct {
	Path      string `json:"path"`
	Added     int    `json:"added"`
	Deleted   int    `json:"deleted"`
	Binary    bool   `json:"binary,omitempty"`
	Untracked bool   `json:"untracked,omitempty"`
}

// PushRunParams runs a ticket's push step. Service limits it to one service.
type PushRunParams struct {
	ID      string `json:"id"`
	Service string `json:"service,omitempty"`
}

// PushRunResult says how each service went. Done is true when every service of the
// ticket has been pushed and the ticket moved to the done phase.
type PushRunResult struct {
	Summary  TicketSummary       `json:"summary"`
	Services []PushServiceResult `json:"services"`
	Done     bool                `json:"done"`
}

// PushServiceResult is one service's push. Skipped means it had already been pushed by
// an earlier run and was left alone; Error means this attempt failed (a later run
// retries it).
type PushServiceResult struct {
	Service string            `json:"service"`
	Mode    domain.PushMode   `json:"mode"`
	Status  domain.PushStatus `json:"status"`
	Skipped bool              `json:"skipped,omitempty"`
	Pushed  bool              `json:"pushed"`
	Ref     string            `json:"ref,omitempty"`
	Sha     string            `json:"sha,omitempty"`
	PRURL   string            `json:"pr_url,omitempty"`
	Note    string            `json:"note,omitempty"`
	Error   string            `json:"error,omitempty"`
}
