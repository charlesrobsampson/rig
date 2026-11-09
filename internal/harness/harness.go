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
	TicketDir string // holds plan.md, tasks.md and contract.md
	Dir       string // the service's checkout: the agent's working directory
	Network   domain.NetworkMode
	// Profile is the resolved auth profile; nil means no credentials are passed.
	Profile *config.AuthProfile
	// Secrets holds resolved secret values by name. Only names the ticket lists in
	// `secrets`, or its auth profile lists, ever reach the environment.
	Secrets map[string]string
	// HookCommand is what the agent's hooks run to report state (default "rig agent-event").
	HookCommand string
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
