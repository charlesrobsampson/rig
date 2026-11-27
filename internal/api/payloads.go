package api

import (
	"encoding/json"
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
	// ContractViolation is set while the agreed section has been edited directly and
	// nobody has resolved it.
	ContractViolation bool `json:"contract_violation,omitempty"`
	// PendingGate is the approval a human still has to give to move the ticket out of
	// its current phase; empty when none is waiting.
	PendingGate domain.Gate `json:"pending_gate,omitempty"`
	// PhaseRequest is an agent's open request to move the ticket, waiting for a human.
	PhaseRequest *PhaseRequestSummary `json:"phase_request,omitempty"`
	// Question is an agent's open question to a person, until a message to that service answers it.
	Question *QuestionSummary `json:"question,omitempty"`
	// Review is where the ticket's review loop stands, once it has begun.
	Review *ReviewSummary `json:"review,omitempty"`
	// AISummary is the language model's note on the ticket, when a summarizer is
	// configured and has produced one. It is advisory.
	AISummary *SummaryInfo `json:"ai_summary,omitempty"`
	// Drift lists, one line each, how reality differs from state.json for this ticket
	// (a container that should be running is gone, a checkout directory is missing).
	// Rig marks drift; it never guesses a repair.
	Drift []string `json:"drift,omitempty"`
	// CostUSD is what all the ticket's agents (its reviewer included) report having
	// spent; absent when none reports cost.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// AgentSeconds is the time the ticket's agents spent working, added up over all of
	// them (as rig stats reports it); absent when none is measured.
	AgentSeconds int64 `json:"agent_seconds,omitempty"`
	// CreatedAt and DoneAt bound the ticket's time (DoneAt once it moved to done).
	// Base lists the services whose base branch has moved on since they branched.
	Base []BaseInfo `json:"base,omitempty"`
	// Touched lists, per service, the files changed against its base (committed and
	// uncommitted); absent for done tickets and services that cannot be measured.
	Touched   []TouchedInfo `json:"touched,omitempty"`
	CreatedAt *time.Time    `json:"created_at,omitempty"`
	DoneAt    *time.Time    `json:"done_at,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type ServiceSummary struct {
	Name       string               `json:"name"`
	Status     domain.ServiceStatus `json:"status,omitempty"`
	AgentState domain.AgentState    `json:"agent_state,omitempty"`
	// CostUSD is what the agent's harness reports having spent this session. It is
	// absent, not zero, when the harness does not report cost.
	CostUSD  *float64 `json:"cost_usd,omitempty"`
	HostPort int      `json:"host_port,omitempty"`
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
	// Read asks for the pane's text as well: a look that types nothing into it.
	Read bool `json:"read,omitempty"`
	// Lines is how many of the last lines to read; 0 means what is visible.
	Lines int `json:"lines,omitempty"`
}

// AgentAttachResult is the command a human runs to see the agent's pane, and with
// Read, what the pane shows.
type AgentAttachResult struct {
	Service string   `json:"service"`
	Command []string `json:"command"`
	Screen  string   `json:"screen,omitempty"`
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
	// Discard throws the work away instead of importing it: the checkouts are removed
	// whatever they hold, and the ticket's branch is deleted from the main repository.
	Discard bool `json:"discard,omitempty"`
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
	// BranchDeleted: a discarding stop deleted the ticket's branch from the main repository.
	BranchDeleted bool `json:"branch_deleted,omitempty"`
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

// ReviewSummary is a ticket's review loop at a glance.
type ReviewSummary struct {
	Status domain.ReviewStatus `json:"status"`
	Round  int                 `json:"round"`
	Reason string              `json:"reason,omitempty"`
	// At is when the loop last changed: a round starting, or a pass, stop or other decision.
	At time.Time `json:"at,omitzero"`
	// Stale: the review passed one commit and the reviewed branch has moved past it.
	Stale bool `json:"stale,omitempty"`
	// Stalled is set while the round is reviewing and a checker that still owes its
	// result has had its agent idle for StalledAfter.
	Stalled *ReviewStalled `json:"stalled,omitempty"`
}

// StalledAfter is how long a checker's agent may sit idle, owing its result, before the
// round is shown as stalled.
const StalledAfter = 5 * time.Minute

// ReviewStalled names the checker (review or tester) a round is waiting on and since when
// its agent has been idle.
type ReviewStalled struct {
	Service string    `json:"service"`
	Since   time.Time `json:"since"`
}

// ReviewerParams names a ticket with a review.
type ReviewerParams struct {
	Ticket string `json:"ticket"`
}

// ReviewerDecideParams is a person's call on a review loop: "pass" lets the ticket move
// on, "stop" ends the loop for a human to look at. Reason is required.
type ReviewerDecideParams struct {
	Ticket  string `json:"ticket"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

// ReviewerStatus is a ticket's review loop in full: each round's findings and the
// coder's answers, and what the loop has cost.
type ReviewerStatus struct {
	Status      domain.ReviewStatus `json:"status,omitempty"`
	Round       int                 `json:"round"`
	MaxRounds   int                 `json:"max_rounds"`
	Commit      string              `json:"commit,omitempty"`
	Base        string              `json:"base,omitempty"`
	Reason      string              `json:"reason,omitempty"`
	By          string              `json:"by,omitempty"`
	BudgetUSD   float64             `json:"budget_usd,omitempty"`
	SpentUSD    float64             `json:"spent_usd"`
	CoderUSD    float64             `json:"coder_usd"`
	ReviewerUSD float64             `json:"reviewer_usd"`
	TesterUSD   float64             `json:"tester_usd,omitempty"`
	Rounds      []ReviewerRound     `json:"rounds,omitempty"`
}

type ReviewerRound struct {
	Round    int             `json:"round"`
	Commit   string          `json:"commit"`
	At       time.Time       `json:"at"`
	Verdict  string          `json:"verdict,omitempty"`
	Findings []ReviewFinding `json:"findings,omitempty"`
	// Tester is the round's tester line, when the ticket has a tester.
	Tester *TesterResult `json:"tester,omitempty"`
}

// TesterResult is a round's test.result record; its findings are numbered T1.. and
// carry the coder's replies like the reviewer's.
type TesterResult struct {
	Outcome  string          `json:"outcome,omitempty"`
	Findings []ReviewFinding `json:"findings,omitempty"`
}

// ReviewFinding is a reviewer's review.result record, with the coder's answer to it.
type ReviewFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	What     string `json:"what"`
	Scenario string `json:"scenario,omitempty"`
	Fix      string `json:"fix,omitempty"`
	// Source is "tester" for a finding from the tester; empty for the reviewer's.
	Source string       `json:"source,omitempty"`
	Reply  *ReviewReply `json:"reply,omitempty"`
}

// ReviewReply is the coder's review.reply record for one finding.
type ReviewReply struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Commit string `json:"commit,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ReviewUpdatedData is the review.updated event's data.
type ReviewUpdatedData struct {
	Status domain.ReviewStatus `json:"status"`
	Round  int                 `json:"round"`
	Commit string              `json:"commit,omitempty"`
	Reason string              `json:"reason,omitempty"`
}

// QuestionSummary is a question an agent asked a person and who asked.
type QuestionSummary struct {
	Service string    `json:"service"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
}

// ReworkParams is a person sending a ticket in test, review or push back to implement
// with something the review missed. Lens names the kind of reviewer that should have
// caught it.
type ReworkParams struct {
	ID      string `json:"id"`
	Finding string `json:"finding"`
	Lens    string `json:"lens,omitempty"`
}

// PhaseRequestSummary is the move an agent asked for and who asked.
type PhaseRequestSummary struct {
	To      domain.Phase `json:"to"`
	Service string       `json:"service"`
	Reason  string       `json:"reason,omitempty"`
	At      time.Time    `json:"at"`
}

// TransitionResult describes the move that was made.
type TransitionResult struct {
	Summary TicketSummary `json:"summary"`
	From    domain.Phase  `json:"from"`
	To      domain.Phase  `json:"to"`
	By      string        `json:"by"`
	Reason  string        `json:"reason"`
	At      time.Time     `json:"at"`
	// Warnings do not stop the move: a checkout with uncommitted work when moving to
	// test or review.
	Warnings []string `json:"warnings,omitempty"`
}

// AgentEventParams is what an agent's hook reports: the agent of ticket/service is now
// in State (ready, working, blocked, idle or exited). "ready" means the agent's
// interface is up and can take typed input; it counts as idle.
type AgentEventParams struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service"`
	// State is ready, working, blocked, idle or exited, or "cost": the harness's
	// running total for this agent session, in CostUSD.
	State   string   `json:"state"`
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// AgentRecordParams is a record a host agent adds to its ticket's journal
// (docs/formats.md §14). The daemon stamps the ticket, service, time and source itself.
type AgentRecordParams struct {
	Ticket  string          `json:"ticket"`
	Service string          `json:"service"`
	Kind    string          `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// JournalSource says who wrote a journal record: an agent in a container (through its
// events file), an agent on the host (through agent.record), or rig itself.
type JournalSource string

const (
	JournalSourceContainer JournalSource = "container"
	JournalSourceHost      JournalSource = "host"
	JournalSourceRig       JournalSource = "rig"
)

// JournalRecord is one line of a ticket's journal.jsonl.
type JournalRecord struct {
	At      time.Time       `json:"at"`
	Ticket  string          `json:"ticket"`
	Service string          `json:"service"`
	Source  JournalSource   `json:"source"`
	Kind    string          `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// JournalListParams filters journal records. An empty Ticket means every ticket; Limit
// keeps only the newest records (0 means all).
type JournalListParams struct {
	Ticket  string `json:"ticket,omitempty"`
	Service string `json:"service,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// JournalListResult holds the matching records, oldest first.
type JournalListResult struct {
	Records []JournalRecord `json:"records"`
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

// TestsRenderParams asks for a ticket's manual tests with live addresses filled in.
type TestsRenderParams struct {
	Ticket string `json:"ticket"`
}

// TestsRenderResult is manual-tests.md rendered: the introduction and each step with its
// placeholders replaced, and whether a human has ticked the step off.
type TestsRenderResult struct {
	Preamble  string      `json:"preamble,omitempty"`
	Steps     []TestsStep `json:"steps"`
	SignedOff bool        `json:"signed_off"`
}

type TestsStep struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Done  bool   `json:"done"`
}

// ContractListParams asks for a ticket's contract proposals.
type ContractListParams struct {
	Ticket string `json:"ticket"`
}

// ContractProposal is one proposed change to the contract.
type ContractProposal struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Status   string `json:"status"` // open, accepted or rejected
	What     string `json:"what"`
	Why      string `json:"why"`
	Rejected string `json:"rejected,omitempty"`
}

// ContractListResult lists the proposals, oldest first, and any problems found in the
// file (a malformed proposal heading, say). Violation is set while the agreed section
// has been edited directly and nobody has acknowledged it.
type ContractListResult struct {
	Ticket    string             `json:"ticket"`
	Proposals []ContractProposal `json:"proposals"`
	Problems  []string           `json:"problems,omitempty"`
	Violation string             `json:"violation,omitempty"`
}

// ContractResolveParams accepts or rejects one open proposal (a human action). Reason
// is required to reject.
type ContractResolveParams struct {
	Ticket   string `json:"ticket"`
	Proposal string `json:"proposal"`
	Reason   string `json:"reason,omitempty"`
}

// ContractResolveResult says what the proposal became.
type ContractResolveResult struct {
	Proposal string `json:"proposal"`
	Outcome  string `json:"outcome"` // accepted or rejected
}

// ContractAckParams clears the direct-edit violation once a human has looked at it.
type ContractAckParams struct {
	Ticket string `json:"ticket"`
}

// TicketFileNames are the lifecycle files a client may read with ticket.file.
var TicketFileNames = []string{"plan.md", "tasks.md", "contract.md", "manual-tests.md"}

// TicketFileParams asks for one of a ticket's lifecycle files.
type TicketFileParams struct {
	Ticket string `json:"ticket"`
	Name   string `json:"name"` // one of TicketFileNames
}

// TicketFileResult is the file's text. An absent file is not_found: contract.md exists
// only for tickets with several services.
type TicketFileResult struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// TestsStepParams ticks a manual-test step off (Done) or back on again. The step id
// must be one of the steps in the ticket's manual-tests.md.
type TestsStepParams struct {
	Ticket string `json:"ticket"`
	Step   string `json:"step"`
	Done   bool   `json:"done"`
}

// TestsStepResult echoes the step. SignedOff is false after unticking a step of a
// ticket that had been signed off: the sign-off no longer holds.
type TestsStepResult struct {
	Step      string `json:"step"`
	Done      bool   `json:"done"`
	SignedOff bool   `json:"signed_off"`
}

// TestsSignoffParams signs off the manual tests (a human action). Every step must be
// ticked unless Override is set, which signs off anyway.
type TestsSignoffParams struct {
	Ticket   string `json:"ticket"`
	Override bool   `json:"override,omitempty"`
}

// TestsSignoffResult says what was recorded. Approved is true when the manual_test_signoff
// gate was approved by this sign-off (it is not, when no service requires that gate).
// Unchecked lists the steps that were not ticked, for an override.
type TestsSignoffResult struct {
	SignedOff bool     `json:"signed_off"`
	Approved  bool     `json:"approved"`
	Unchecked []string `json:"unchecked,omitempty"`
}

// SummaryInfo is a ticket's language-model summary, with the raw diff stats it was made
// from beside it (the summary never replaces them).
type SummaryInfo struct {
	Text    string `json:"text,omitempty"`
	Backend string `json:"backend"` // ollama or claude
	Model   string `json:"model,omitempty"`
	Remote  bool   `json:"remote"` // the content was sent off this machine
	// At is when Text was generated.
	At      *time.Time `json:"at,omitempty"`
	Files   int        `json:"files"`
	Added   int        `json:"added"`
	Deleted int        `json:"deleted"`
	// Error is the last failure to refresh the summary; Text, when present, is the
	// previous one.
	Error string `json:"error,omitempty"`
}

// ContractProposeParams appends a proposed change to a ticket's contract on behalf of a
// service (what its agent would write by hand). It never touches the agreed section; a
// human decides.
type ContractProposeParams struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service"` // the proposing service
	What    string `json:"what"`
	Why     string `json:"why"`
}

// ContractProposeResult names the new proposal.
type ContractProposeResult struct {
	Proposal string `json:"proposal"`
}

// ReconcileParams asks the daemon to compare state.json with what is really there. With
// Fix it also removes orphans: rig-labelled containers and networks that belong to no
// ticket, or to a ticket that is stopped. Nothing else is ever touched.
type ReconcileParams struct {
	Fix bool `json:"fix,omitempty"`
}

// DriftItem is one difference between state.json and reality.
type DriftItem struct {
	Ticket  string `json:"ticket"`
	Service string `json:"service,omitempty"`
	// Kind is one of: checkout_missing, service_container_missing,
	// dependency_container_missing, agent_container_missing, agent_pane_missing,
	// network_missing, orphan_container, orphan_network.
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	// Fixable is true for orphans, which `reconcile --fix` removes.
	Fixable bool `json:"fixable,omitempty"`
}

// ReconcileResult is the drift found, and with Fix what was removed and what failed.
// Notes say what could not be checked (Docker unreachable, no terminal backend).
type ReconcileResult struct {
	Drift    []DriftItem `json:"drift"`
	Fixed    []string    `json:"fixed,omitempty"`
	Failures []string    `json:"failures,omitempty"`
	Notes    []string    `json:"notes,omitempty"`
}

// StatsParams asks for the usage of some tickets (all of them when IDs is empty).
type StatsParams struct {
	IDs []string `json:"ids,omitempty"`
}

// TokenCount is what a model read and wrote (usage.Tokens on the wire).
type TokenCount struct {
	Input      int64 `json:"input"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
	Output     int64 `json:"output"`
}

// AgentStats is one agent of a ticket: the coder of a service, or the reviewer.
type AgentStats struct {
	Service string     `json:"service"`
	Role    string     `json:"role"` // coder, reviewer or tester
	CostUSD float64    `json:"cost_usd"`
	Tokens  TokenCount `json:"tokens"`
	// Requests is how many model requests its transcripts hold; 0 with Tokens empty
	// means rig has no transcripts for it (a host agent on the person's own login).
	Requests int      `json:"requests"`
	Models   []string `json:"models,omitempty"`
	// AgentSeconds is the time the agent spent working (usage.WorkGap).
	AgentSeconds int64 `json:"agent_seconds"`
}

// PhaseTime is how long a ticket spent in a phase, all visits together.
type PhaseTime struct {
	Phase   domain.Phase `json:"phase"`
	Seconds int64        `json:"seconds"`
}

// GateWait is how long an approval took after an agent asked to move on.
type GateWait struct {
	Gate    domain.Gate `json:"gate"`
	Seconds int64       `json:"seconds"`
}

// ChangeStats is the size of a service's change against its base.
type ChangeStats struct {
	Files   int `json:"files"`
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
}

// TicketStats is everything rig measured about one ticket.
type TicketStats struct {
	ID      string       `json:"id"`
	Title   string       `json:"title"`
	Phase   domain.Phase `json:"phase"`
	CostUSD float64      `json:"cost_usd"`
	Tokens  TokenCount   `json:"tokens"`
	Agents  []AgentStats `json:"agents"`
	// AgentSeconds adds up the agents' working time; it exceeds ElapsedSeconds when
	// agents ran in parallel.
	AgentSeconds int64 `json:"agent_seconds"`
	// ElapsedSeconds runs from the ticket's creation to its move to done (or now).
	CreatedAt      time.Time   `json:"created_at"`
	DoneAt         *time.Time  `json:"done_at,omitempty"`
	ElapsedSeconds int64       `json:"elapsed_seconds"`
	Phases         []PhaseTime `json:"phases"`
	// WaitingOnPeopleSeconds adds up Gates: time between an agent asking to move on and a
	// person approving.
	WaitingOnPeopleSeconds int64                  `json:"waiting_on_people_seconds"`
	Gates                  []GateWait             `json:"gates,omitempty"`
	ReviewRounds           int                    `json:"review_rounds"`
	ReviewStatus           domain.ReviewStatus    `json:"review_status,omitempty"`
	Changes                map[string]ChangeStats `json:"changes,omitempty"`
	Error                  string                 `json:"error,omitempty"`
}

// StatsTotals adds up the tickets of a vessel.
type StatsTotals struct {
	Tickets        int        `json:"tickets"`
	Done           int        `json:"done"`
	CostUSD        float64    `json:"cost_usd"`
	Tokens         TokenCount `json:"tokens"`
	ElapsedSeconds int64      `json:"elapsed_seconds"`
	AgentSeconds   int64      `json:"agent_seconds"`
	ReviewRounds   int        `json:"review_rounds"`
	Added          int        `json:"added"`
	Deleted        int        `json:"deleted"`
}

type StatsResult struct {
	Tickets []TicketStats `json:"tickets"`
	Totals  StatsTotals   `json:"totals"`
}

// TouchedInfo is the files one service's checkout has changed against its base.
type TouchedInfo struct {
	Service string   `json:"service"`
	Files   []string `json:"files"`
}

// BaseInfo is how one service's branch stands against its base branch.
type BaseInfo struct {
	Service   string   `json:"service"`
	Base      string   `json:"base"`
	Behind    int      `json:"behind"`
	Conflicts []string `json:"conflicts,omitempty"`
}

type SyncParams struct {
	ID      string `json:"id"`
	Service string `json:"service,omitempty"`
}

// SyncService is what syncing one service did.
type SyncService struct {
	BaseInfo
	// Merged is the merge commit; empty when the base had nothing new, or when the
	// merge conflicted and is left to the agent.
	Merged string `json:"merged,omitempty"`
}

type SyncResult struct {
	Services []SyncService `json:"services"`
	// Phase is where the ticket is after the sync (a ticket past implement goes back).
	Phase domain.Phase `json:"phase"`
	// ReviewStarted: a review round began on the merged work.
	ReviewStarted bool `json:"review_started,omitempty"`
}

// TimelineListParams asks for a ticket's timeline after Since (0 for all of it).
type TimelineListParams struct {
	Ticket string `json:"ticket"`
	Since  int    `json:"since,omitempty"`
}

// TimelineEntry is one line of a ticket's timeline.jsonl (docs/formats.md §15).
type TimelineEntry struct {
	Seq     int             `json:"seq"`
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	By      string          `json:"by,omitempty"`
	Service string          `json:"service,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// TimelineListResult holds the entries, oldest first; empty when there are none.
type TimelineListResult struct {
	Entries []TimelineEntry `json:"entries"`
}

// EventsSubscribeParams narrows events.subscribe (docs/formats.md §8). Both fields are
// optional: no params streams every event from now on.
type EventsSubscribeParams struct {
	// Tickets limits the stream to these tickets' events (an event with no ticket, such
	// as the subscribed ack, always passes). Empty means every ticket.
	Tickets []string `json:"tickets,omitempty"`
	// Since asks for a replay: for each ticket, every timeline entry after that seq is
	// sent first, then the live stream follows without a gap or a repeat. 0 replays
	// the whole timeline. A ticket named here is also watched, as if in Tickets.
	Since map[string]int `json:"since,omitempty"`
}

// TimelineEntryData is the data of a timeline.entry event.
type TimelineEntryData struct {
	Ticket string        `json:"ticket"`
	Seq    int           `json:"seq"`
	Entry  TimelineEntry `json:"entry"`
}

// DaemonBusyResult lists what the daemon is part way through, which a restart would cut off.
type DaemonBusyResult struct {
	Tickets []InFlight `json:"tickets"`
}

// InFlight is one ticket with work under way and what that work is.
type InFlight struct {
	Ticket string `json:"ticket"`
	Doing  string `json:"doing"`
}
