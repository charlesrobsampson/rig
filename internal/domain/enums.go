package domain

import (
	"fmt"
	"slices"
	"strings"
)

type Phase string

const (
	PhasePlan      Phase = "plan"
	PhaseImplement Phase = "implement"
	PhaseTest      Phase = "test"
	PhaseReview    Phase = "review"
	PhasePush      Phase = "push"
	PhaseDone      Phase = "done"
)

var phases = []Phase{PhasePlan, PhaseImplement, PhaseTest, PhaseReview, PhasePush, PhaseDone}

func (p Phase) Valid() bool                   { return slices.Contains(phases, p) }
func (p Phase) MarshalText() ([]byte, error)  { return marshalEnum("phase", p, p.Valid()) }
func (p *Phase) UnmarshalText(b []byte) error { return unmarshalEnum("phase", phases, b, p) }

type ServiceStatus string

const (
	ServicePending     ServiceStatus = "pending"
	ServiceProvisioned ServiceStatus = "provisioned"
	ServiceRunning     ServiceStatus = "running"
	ServiceStopped     ServiceStatus = "stopped"
	ServiceError       ServiceStatus = "error"
)

var serviceStatuses = []ServiceStatus{ServicePending, ServiceProvisioned, ServiceRunning, ServiceStopped, ServiceError}

func (s ServiceStatus) Valid() bool { return slices.Contains(serviceStatuses, s) }
func (s ServiceStatus) MarshalText() ([]byte, error) {
	return marshalEnum("service status", s, s.Valid())
}
func (s *ServiceStatus) UnmarshalText(b []byte) error {
	return unmarshalEnum("service status", serviceStatuses, b, s)
}

type AgentState string

const (
	AgentWorking AgentState = "working"
	AgentBlocked AgentState = "blocked"
	AgentIdle    AgentState = "idle"
	AgentUnknown AgentState = "unknown"
	AgentExited  AgentState = "exited"
)

var agentStates = []AgentState{AgentWorking, AgentBlocked, AgentIdle, AgentUnknown, AgentExited}

func (a AgentState) Valid() bool                  { return slices.Contains(agentStates, a) }
func (a AgentState) MarshalText() ([]byte, error) { return marshalEnum("agent state", a, a.Valid()) }
func (a *AgentState) UnmarshalText(b []byte) error {
	return unmarshalEnum("agent state", agentStates, b, a)
}

type PushStatus string

const (
	PushPending PushStatus = "pending"
	PushDone    PushStatus = "done"
	PushFailed  PushStatus = "failed"
)

var pushStatuses = []PushStatus{PushPending, PushDone, PushFailed}

func (s PushStatus) Valid() bool                  { return slices.Contains(pushStatuses, s) }
func (s PushStatus) MarshalText() ([]byte, error) { return marshalEnum("push status", s, s.Valid()) }
func (s *PushStatus) UnmarshalText(b []byte) error {
	return unmarshalEnum("push status", pushStatuses, b, s)
}

type PushMode string

const (
	PushNone   PushMode = "none"
	PushBranch PushMode = "push-branch"
	PushPR     PushMode = "pr"
	PushMerge  PushMode = "merge"
)

var pushModes = []PushMode{PushNone, PushBranch, PushPR, PushMerge}

func (m PushMode) Valid() bool                   { return slices.Contains(pushModes, m) }
func (m PushMode) MarshalText() ([]byte, error)  { return marshalEnum("push mode", m, m.Valid()) }
func (m *PushMode) UnmarshalText(b []byte) error { return unmarshalEnum("push mode", pushModes, b, m) }

type Sandbox string

const (
	SandboxContainer Sandbox = "container"
	SandboxHost      Sandbox = "host"
)

var sandboxes = []Sandbox{SandboxContainer, SandboxHost}

func (s Sandbox) Valid() bool                   { return slices.Contains(sandboxes, s) }
func (s Sandbox) MarshalText() ([]byte, error)  { return marshalEnum("sandbox", s, s.Valid()) }
func (s *Sandbox) UnmarshalText(b []byte) error { return unmarshalEnum("sandbox", sandboxes, b, s) }

type NetworkMode string

const (
	NetworkBridge   NetworkMode = "bridge"
	NetworkInternal NetworkMode = "internal"
)

var networkModes = []NetworkMode{NetworkBridge, NetworkInternal}

func (n NetworkMode) Valid() bool { return slices.Contains(networkModes, n) }
func (n NetworkMode) MarshalText() ([]byte, error) {
	return marshalEnum("network mode", n, n.Valid())
}
func (n *NetworkMode) UnmarshalText(b []byte) error {
	return unmarshalEnum("network mode", networkModes, b, n)
}

type Gate string

const (
	GatePlanApproval      Gate = "plan_approval"
	GateReviewApproval    Gate = "review_approval"
	GateManualTestSignoff Gate = "manual_test_signoff"
)

var gates = []Gate{GatePlanApproval, GateReviewApproval, GateManualTestSignoff}

func (g Gate) Valid() bool                   { return slices.Contains(gates, g) }
func (g Gate) MarshalText() ([]byte, error)  { return marshalEnum("gate", g, g.Valid()) }
func (g *Gate) UnmarshalText(b []byte) error { return unmarshalEnum("gate", gates, b, g) }

func marshalEnum[T ~string](kind string, v T, valid bool) ([]byte, error) {
	if !valid {
		return nil, fmt.Errorf("invalid %s %q", kind, string(v))
	}
	return []byte(v), nil
}

func unmarshalEnum[T ~string](kind string, valid []T, text []byte, out *T) error {
	v := T(text)
	if !slices.Contains(valid, v) {
		names := make([]string, len(valid))
		for i, n := range valid {
			names[i] = string(n)
		}
		return fmt.Errorf("invalid %s %q (want one of: %s)", kind, text, strings.Join(names, ", "))
	}
	*out = v
	return nil
}
