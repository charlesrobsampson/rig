// Package harness turns a ticket's service into the command that launches its agent:
// the program and flags, the environment (rig's own variables, the auth profile, the
// ticket's env), and the prompt that seeds it. The terminal backend only runs what a
// harness produces (manifest §6, docs/formats.md §13).
package harness

import (
	"fmt"
	"strings"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// Request is everything a launch depends on.
type Request struct {
	Ticket    domain.Ticket
	Service   string
	Phase     domain.Phase
	TicketDir string // holds plan.md, tasks.md and contract.md, as rig reads them
	// AgentTicketDir is where the agent sees those files (a container mounts them
	// elsewhere); empty means TicketDir.
	AgentTicketDir string
	Dir            string // the service's checkout: the agent's working directory
	Network        domain.NetworkMode
	// Profile is the resolved auth profile; nil means no credentials are passed.
	Profile *config.AuthProfile
	// Secrets holds resolved secret values by name. Only names the ticket lists in
	// `secrets`, or its auth profile lists, ever reach the environment.
	Secrets map[string]string
	// HookCommand is what the agent's hooks run to report state (default "rig agent-event").
	HookCommand string
	// CostCommand is what the harness's status line runs to report the agent's running
	// cost. Empty means cost is not reported (the readout stays absent, never zero).
	CostCommand string
	// ConfigDir is the agent's own harness configuration directory, as the agent sees
	// it. Empty means the agent uses its user's own configuration.
	ConfigDir string
	// Model is passed as --model; empty leaves Claude Code's default.
	Model string
	// ReviewOf names the service this agent reviews: it gets a reviewer's prompt.
	ReviewOf string
	// TestOf names the service this agent tests: it gets a tester's prompt, headed by
	// TestBrief (the brief file's text) or rig's default brief when that is empty.
	TestOf, TestBrief string
	// Reviewed says a reviewer checks this agent's work, which changes how it finishes.
	Reviewed bool
}

// Launch is what a terminal backend needs to start the agent.
type Launch struct {
	Command []string
	Env     map[string]string
	Dir     string
}

type Harness interface {
	Name() string
	Command(Request) (Launch, error)
	// AuthEnv maps an auth profile to the environment variables this harness reads.
	AuthEnv(p config.AuthProfile, secrets map[string]string) (map[string]string, error)
	// PrepareConfig readies a configuration directory so the agent starts without
	// first-run questions, trusts exactly the given (real) paths, and has the
	// templates' instructions, subagents, skills and hooks.
	PrepareConfig(dir string, c Config) error
	// CheckTemplates reports why configuration templates must not reach an agent.
	CheckTemplates(templates []string, secrets map[string]string) error
}

// Get returns the harness for a ticket's `harness` value; "" means claude-code.
func Get(name string) (Harness, error) {
	switch name {
	case "", "claude-code":
		return ClaudeCode{}, nil
	}
	return nil, fmt.Errorf("unknown harness %q (supported: claude-code)", name)
}

func indent(s string) string { return strings.TrimRight(s, "\n") }

// Config is what PrepareConfig puts in an agent's configuration directory.
type Config struct {
	// Trusted are the real paths the agent may work in without a trust prompt.
	Trusted []string
	// Templates are directories laid out like the harness's user configuration
	// (~/.claude for Claude Code). They are copied, in order, into Snapshot, and a later
	// template's file replaces an earlier one's. The templates themselves are never changed.
	Templates []string
	// Snapshot is the rig-owned copy of the templates on the host, rebuilt on every
	// launch; SnapshotSeen is the same directory as the agent sees it, which the
	// configuration directory links to.
	Snapshot, SnapshotSeen string
	// Secrets are the values, by name, that no template may contain.
	Secrets map[string]string
}
