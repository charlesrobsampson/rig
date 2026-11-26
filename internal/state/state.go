// Package state reads and writes a ticket's state.json: the source of truth for its
// phase, approvals, ports, and runtime ids (docs/formats.md §5).
package state

import (
	"encoding/json"
	"time"

	"github.com/charlesrobsampson/rig/internal/domain"
)

const SchemaVersion = 1

type State struct {
	SchemaVersion int                        `json:"schema_version"`
	TicketID      string                     `json:"ticket_id"`
	Phase         domain.Phase               `json:"phase"`
	Approvals     []Approval                 `json:"approvals"`
	Transitions   []Transition               `json:"transitions"`
	Services      map[string]ServiceState    `json:"services"`
	Dependencies  map[string]DependencyState `json:"dependencies"`
	Network       Network                    `json:"network"`
	// EgressNetwork is the proxy's outward network, present only in internal mode.
	EgressNetwork *Network    `json:"egress_network,omitempty"`
	Contract      Contract    `json:"contract"`
	ManualTests   ManualTests `json:"manual_tests"`
	// PhaseRequest is an agent's open request to move the ticket, waiting for a human;
	// any move clears it.
	PhaseRequest *PhaseRequest `json:"phase_request,omitempty"`
	// Question is an agent's open question to a person; a person's message to that
	// service answers it.
	Question *Question `json:"question,omitempty"`
	// Review is the ticket's review loop, when its ticket.yaml has a review.
	Review *Review `json:"review,omitempty"`
	// SyncReturn is the phase a sync moved a reviewed ticket back from; when a round
	// passes on the merged work rig moves the ticket there. Any other move clears it.
	SyncReturn domain.Phase `json:"sync_return,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`

	unknown map[string]json.RawMessage
}

// Approval.Service is empty when the approval covers every service.
type Approval struct {
	Gate    domain.Gate `json:"gate"`
	Service string      `json:"service"`
	By      string      `json:"by"`
	At      time.Time   `json:"at"`
}

// Question is what an agent asked a person through its journal.
type Question struct {
	Service string    `json:"service"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
}

// PhaseRequest is what an agent asked for through its journal.
type PhaseRequest struct {
	To      domain.Phase `json:"to"`
	Service string       `json:"service"`
	Reason  string       `json:"reason,omitempty"`
	At      time.Time    `json:"at"`
}

// Review is where a ticket's review loop stands. A backward move to implement starts a
// new loop, so Round counts within one loop.
type Review struct {
	Status domain.ReviewStatus `json:"status"`
	Round  int                 `json:"round"`
	// Commit is the coder's commit the current round reviews, and Base where its
	// change starts (the merge base with the service's base branch).
	Commit string `json:"commit,omitempty"`
	Base   string `json:"base,omitempty"`
	// Reason says why it passed or stopped; By says who decided ("rig" for a cap).
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`
	// CostAtStart is what the ticket's agents had spent when the loop began, so the
	// budget counts only the loop.
	CostAtStart float64 `json:"cost_at_start"`
	// Asked says the loop stopped for the coder's question to a person: a person's
	// start continues it instead of beginning a new loop. Uncounted is how many rounds
	// ended that way; they keep their numbers but do not count toward max_rounds.
	Asked     bool          `json:"asked,omitempty"`
	Uncounted int           `json:"uncounted,omitempty"`
	Rounds    []ReviewRound `json:"rounds,omitempty"`
	At        time.Time     `json:"at"`
}

type ReviewRound struct {
	Round  int       `json:"round"`
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
	// Verdict is the reviewer's closing line for the round, once it has one.
	Verdict string `json:"verdict,omitempty"`
	// ReviewerDone says the reviewer's verdict is in, and Tester is the tester's
	// outcome once it has one; a round with a tester settles when both are.
	ReviewerDone bool         `json:"reviewer_done,omitempty"`
	Tester       *TesterRound `json:"tester,omitempty"`
}

type TesterRound struct {
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
}

// Transition.By is "human", "agent:<service>" or "rig"; Reason is required for backward moves.
type Transition struct {
	From   domain.Phase `json:"from"`
	To     domain.Phase `json:"to"`
	By     string       `json:"by"`
	Reason string       `json:"reason"`
	At     time.Time    `json:"at"`
}

type ServiceState struct {
	Status      domain.ServiceStatus `json:"status"`
	Checkout    Checkout             `json:"checkout"`
	Branch      string               `json:"branch"`
	Base        string               `json:"base"`
	HostPort    int                  `json:"host_port"`
	ContainerID string               `json:"container_id"`
	Agent       Agent                `json:"agent"`
	Push        Push                 `json:"push"`
	Error       string               `json:"error"`

	unknown map[string]json.RawMessage
}

type Checkout struct {
	Kind domain.CheckoutKind `json:"kind,omitempty"`
	Path string              `json:"path"`
}

type Agent struct {
	Backend     string            `json:"backend"`
	PaneID      string            `json:"pane_id"`
	State       domain.AgentState `json:"state"`
	ContainerID string            `json:"container_id"`
	// Cost is what the agent's harness last reported spending. It is absent (nil) when
	// the harness does not report cost, never a made-up zero.
	Cost *Cost `json:"cost,omitempty"`
}

// Cost is a harness's own running total for one agent session, in US dollars.
type Cost struct {
	USD float64   `json:"usd"`
	At  time.Time `json:"at"`
}

type Push struct {
	Status domain.PushStatus `json:"status"`
	PRURL  string            `json:"pr_url"`
	Error  string            `json:"error"`
	// Files, Added and Deleted are the size of the change that was pushed, kept because
	// the checkout (and often the branch) is gone once the ticket is done.
	Files   int `json:"files,omitempty"`
	Added   int `json:"added,omitempty"`
	Deleted int `json:"deleted,omitempty"`
}

type DependencyState struct {
	ContainerID string               `json:"container_id"`
	Status      domain.ServiceStatus `json:"status"`
}

type Network struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

type Contract struct {
	Pending bool `json:"pending"`
	// AgreedHash is the hash of the agreed section as rig last knew it to be right; a
	// different hash after the plan is approved means someone edited it directly.
	AgreedHash string `json:"agreed_hash,omitempty"`
	// Seen lists the proposal ids already announced, so each is announced once, even
	// across daemon restarts.
	Seen []string `json:"seen,omitempty"`
	// Resolved lists proposals a human resolved through rig, whose contract.resolved
	// event rig already published, so the file watcher does not publish it again.
	Resolved []string `json:"resolved,omitempty"`
	// Violation describes an unresolved direct edit of the agreed section.
	Violation string `json:"violation,omitempty"`
}

type ManualTests struct {
	Steps     map[string]bool `json:"steps"`
	SignedOff bool            `json:"signed_off"`
}

func (s *State) UnmarshalJSON(data []byte) error {
	type plain State
	if err := json.Unmarshal(data, (*plain)(s)); err != nil {
		return err
	}
	s.unknown = unknownFields(data, plain{})
	return nil
}

func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	return marshalWithUnknown(plain(s), s.unknown)
}

func (s *ServiceState) UnmarshalJSON(data []byte) error {
	type plain ServiceState
	if err := json.Unmarshal(data, (*plain)(s)); err != nil {
		return err
	}
	s.unknown = unknownFields(data, plain{})
	return nil
}

func (s ServiceState) MarshalJSON() ([]byte, error) {
	type plain ServiceState
	return marshalWithUnknown(plain(s), s.unknown)
}

// Active counts against the limit on active tickets: it is in use and not done.
func (s State) Active() bool { return s.Phase != domain.PhaseDone && s.InUse() }

// InUse reports whether the ticket has anything running or provisioned, according to
// this state: it has been started and not stopped since (done or not).
func (s State) InUse() bool {
	if s.Network.Name != "" || s.EgressNetwork != nil {
		return true
	}
	for _, ss := range s.Services {
		switch ss.Status {
		case domain.ServiceProvisioned, domain.ServiceRunning, domain.ServiceError:
			return true
		}
		if ss.ContainerID != "" || ss.Agent.PaneID != "" || ss.Agent.ContainerID != "" {
			return true
		}
	}
	for _, ds := range s.Dependencies {
		if ds.ContainerID != "" {
			return true
		}
	}
	return false
}
