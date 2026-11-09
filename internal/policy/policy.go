// Package policy resolves a service's effective settings: command-line flags beat
// the service, which beats the ticket, which beats the global defaults.
package policy

import (
	"slices"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// Flags are per-invocation overrides. A zero field, or a nil Gates, means "not given";
// an empty non-nil Gates means "no gates".
type Flags struct {
	Gates           []domain.Gate
	PushMode        domain.PushMode
	Sandbox         domain.Sandbox
	NetworkMode     domain.NetworkMode
	Harness         string
	TerminalBackend string
}

type Effective struct {
	Gates    []domain.Gate
	PushMode domain.PushMode
	// MergeCommit: merge mode may create a merge commit (default false).
	MergeCommit     bool
	Sandbox         domain.Sandbox
	NetworkMode     domain.NetworkMode
	Harness         string
	TerminalBackend string
	// Auth is the auth profile name, empty when none is configured anywhere.
	Auth string
}

func Resolve(g config.Global, t domain.Ticket, s domain.Service, f Flags) Effective {
	d := g.Defaults
	return Effective{
		Gates:           firstGates(f.Gates, s.Policy.Gates, t.Policy.Gates, d.Policy.Gates),
		PushMode:        first(f.PushMode, s.Policy.Push.Mode, t.Policy.Push.Mode, d.Policy.Push.Mode),
		MergeCommit:     firstBool(s.Policy.Push.MergeCommit, t.Policy.Push.MergeCommit, d.Policy.Push.MergeCommit),
		Sandbox:         first(f.Sandbox, s.Sandbox, t.Sandbox, d.Sandbox),
		NetworkMode:     first(f.NetworkMode, t.Docker.NetworkMode, d.NetworkMode),
		Harness:         first(f.Harness, s.Harness, t.Harness, d.Harness),
		TerminalBackend: first(f.TerminalBackend, d.TerminalBackend),
		Auth:            first(s.Auth, t.Auth, g.Auth.Default),
	}
}

// firstBool returns the first level that says anything; false when none does.
func firstBool(levels ...*bool) bool {
	for _, v := range levels {
		if v != nil {
			return *v
		}
	}
	return false
}

func first[T comparable](levels ...T) T {
	var zero T
	for _, v := range levels {
		if v != zero {
			return v
		}
	}
	return zero
}

// firstGates takes the first level that is set at all, including an empty list,
// and returns a copy so callers cannot alter the config.
func firstGates(levels ...[]domain.Gate) []domain.Gate {
	for _, l := range levels {
		if l != nil {
			return slices.Clone(l)
		}
	}
	return []domain.Gate{}
}
