// Package terminal is how rig runs and watches agents in terminal panes. A Backend
// (Herdr, tmux, or the in-memory fake) offers panes, the text typed into them, and
// what can be seen of them, and nothing about what an agent is thinking: whether it is
// working or blocked comes from the agent's own hooks (docs/spikes/03-tmux.md §2),
// which the backend merely carries a guess for.
package terminal

import (
	"context"
	"errors"

	"github.com/charlesrobsampson/rig/internal/domain"
)

var (
	// ErrNotFound: no such pane, it was closed, or its process has ended.
	ErrNotFound = errors.New("pane not found")
	// ErrUnavailable: the backend is not installed or cannot be started or reached.
	ErrUnavailable = errors.New("terminal backend unavailable")
	// ErrInvalid: the request itself is wrong, such as a working directory that does not exist.
	ErrInvalid = errors.New("invalid terminal request")
	// ErrExists: the ticket already has a pane for that service.
	ErrExists = errors.New("pane already exists")
)

// PaneID names a pane within one backend. Its form is the backend's business.
type PaneID string

type SpawnSpec struct {
	Ticket  string // groups a ticket's panes (a Herdr workspace, a tmux session)
	Service string // names the pane within the ticket (a tab, a window)
	Dir     string // the working directory; it must exist
	Command []string
	Env     map[string]string
}

type Pane struct {
	ID      PaneID
	Ticket  string
	Service string
	// Alive is false once the process has ended. Some backends keep such a pane listed
	// (so its exit status can be read); others drop it, and Get then returns ErrNotFound.
	// Either way, rig treats the agent as exited.
	Alive bool
	// ExitStatus is the process's exit code when it is known.
	ExitStatus *int
	// Heuristic is a best guess at the agent's state read off the screen, or
	// domain.AgentUnknown. It is only a fallback for agents without hooks.
	Heuristic domain.AgentState
}

type Backend interface {
	// Ready makes the backend usable, starting what it needs (such as a headless
	// server). It is safe to call repeatedly. It returns ErrUnavailable if it cannot.
	Ready(ctx context.Context) error

	// Spawn starts Command in a new pane. It fails with ErrInvalid for a missing
	// directory or empty ticket or service, and ErrExists if the ticket already has a
	// pane for the service; a failed Spawn leaves nothing behind.
	Spawn(ctx context.Context, spec SpawnSpec) (PaneID, error)
	List(ctx context.Context) ([]Pane, error)
	Get(ctx context.Context, id PaneID) (Pane, error)

	// Read returns the last lines of the pane's text (lines <= 0 means what is visible).
	Read(ctx context.Context, id PaneID, lines int) (string, error)
	// SendText types text into the pane exactly as given, including newlines, without
	// pressing Enter. Submit presses Enter. They are separate so the caller decides when
	// to submit; neither is safe to use on an agent that is blocked on a prompt.
	SendText(ctx context.Context, id PaneID, text string) error
	Submit(ctx context.Context, id PaneID) error

	Close(ctx context.Context, id PaneID) error
	// Attach returns the command a human runs to look at the pane.
	Attach(ctx context.Context, id PaneID) ([]string, error)
}
