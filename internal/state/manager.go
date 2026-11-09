package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/fsutil"
)

var (
	ErrNotFound = errors.New("state file not found")
	ErrExists   = errors.New("state file already exists")
)

// Manager reads and writes state files. Every write holds a lock on the file's
// sibling .lock, so concurrent updates from goroutines or processes never lose each
// other's changes.
type Manager struct{ clock clock.Clock }

func NewManager(c clock.Clock) *Manager { return &Manager{clock: c} }

// Create writes a new state file, stamping both timestamps and defaulting the phase
// to plan. It fails if the file exists.
func (m *Manager) Create(path string, s State) error {
	if err := domain.ValidateTicketID(s.TicketID); err != nil {
		return err
	}
	return fsutil.WithLock(path+".lock", func() error {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		now := m.clock.Now().UTC()
		s.CreatedAt, s.UpdatedAt = now, now
		return write(path, s)
	})
}

func (m *Manager) Load(path string) (State, error) { return load(path) }

// Update runs fn on the current state and saves the result, refreshing updated_at.
// If fn fails, nothing is written.
func (m *Manager) Update(path string, fn func(*State) error) error {
	return fsutil.WithLock(path+".lock", func() error {
		s, err := load(path)
		if err != nil {
			return err
		}
		if err := fn(&s); err != nil {
			return err
		}
		s.UpdatedAt = m.clock.Now().UTC()
		return write(path, s)
	})
}

func load(path string) (State, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return State{}, fmt.Errorf("read %s: %w", path, err)
	}
	var version struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return State{}, fmt.Errorf("read %s: %w", path, err)
	}
	switch {
	case version.SchemaVersion < SchemaVersion:
		return State{}, fmt.Errorf("%s has schema version %d, older than the supported %d", path, version.SchemaVersion, SchemaVersion)
	case version.SchemaVersion > SchemaVersion:
		return State{}, fmt.Errorf("%s has schema version %d, newer than the supported %d; upgrade rig", path, version.SchemaVersion, SchemaVersion)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, fmt.Errorf("read %s: %w", path, err)
	}
	return s, nil
}

func write(path string, s State) error {
	withDefaults(&s)
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return fsutil.AtomicWrite(path, append(raw, '\n'))
}

// withDefaults makes a freshly built State writable: unset enums get their initial
// value, and nil lists and maps are written as [] and {} (never null).
func withDefaults(s *State) {
	s.SchemaVersion = SchemaVersion
	if s.Phase == "" {
		s.Phase = domain.PhasePlan
	}
	if s.Approvals == nil {
		s.Approvals = []Approval{}
	}
	if s.Transitions == nil {
		s.Transitions = []Transition{}
	}
	if s.Services == nil {
		s.Services = map[string]ServiceState{}
	}
	if s.Dependencies == nil {
		s.Dependencies = map[string]DependencyState{}
	}
	if s.ManualTests.Steps == nil {
		s.ManualTests.Steps = map[string]bool{}
	}
	for name, svc := range s.Services {
		if svc.Status == "" {
			svc.Status = domain.ServicePending
		}
		if svc.Agent.State == "" {
			svc.Agent.State = domain.AgentUnknown
		}
		if svc.Push.Status == "" {
			svc.Push.Status = domain.PushPending
		}
		s.Services[name] = svc
	}
	for name, dep := range s.Dependencies {
		if dep.Status == "" {
			dep.Status = domain.ServicePending
		}
		s.Dependencies[name] = dep
	}
}

// SetPush records the outcome of pushing one service: the status, the pull request URL
// when there is one, or the error. A later result replaces the earlier one.
func (m *Manager) SetPush(path, service string, push Push) error {
	return m.Update(path, func(s *State) error {
		ss, ok := s.Services[service]
		if !ok {
			return fmt.Errorf("ticket %q has no service %q", s.TicketID, service)
		}
		ss.Push = push
		s.Services[service] = ss
		return nil
	})
}
