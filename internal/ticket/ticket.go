// Package ticket is where new tickets come from: a Source turns a reference (a Jira
// key, an issue number, or just a title) into the seed of a ticket.
package ticket

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charlesrobsampson/rig/internal/domain"
)

var (
	ErrNotFound      = errors.New("ticket not found at its source")
	ErrUnknownSource = errors.New("unknown ticket source")
	ErrUnusable      = errors.New("the source gave an unusable ticket")
)

// Seed is what a source knows about a ticket before rig creates it.
type Seed struct {
	ID          string
	Title       string
	Description string
	URL         string
}

func (s Seed) Validate() error {
	if err := domain.ValidateTicketID(s.ID); err != nil {
		return err
	}
	if strings.TrimSpace(s.Title) == "" {
		return errors.New("a ticket needs a title")
	}
	return nil
}

type Source interface {
	Fetch(ctx context.Context, ref string) (Seed, error)
}

// SanitizeID turns arbitrary text, such as a Jira key or a title, into a valid ticket
// id: lowercase letters and digits joined by single dashes, at most 63 characters.
// It returns "" when nothing usable is left.
func SanitizeID(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	id := b.String()
	if len(id) > 63 {
		id = id[:63]
	}
	return strings.TrimRight(id, "-")
}

// Manual is the source for tickets with no tracker behind them. Fields set from
// command-line flags are used as given; a missing title or id is derived from ref.
type Manual struct {
	ID          string
	Title       string
	Description string
}

func (m Manual) Fetch(_ context.Context, ref string) (Seed, error) {
	ref = strings.TrimSpace(ref)
	seed := Seed{ID: m.ID, Title: m.Title, Description: m.Description}
	if seed.Title == "" {
		seed.Title = ref
	}
	if seed.ID == "" {
		if ref == "" {
			return Seed{}, errors.New("a manual ticket needs an id or a title")
		}
		if seed.ID = SanitizeID(ref); seed.ID == "" {
			return Seed{}, fmt.Errorf("cannot derive a ticket id from %q; give one explicitly", ref)
		}
	}
	// An explicit id is validated, never rewritten: the user typed exactly that.
	if err := seed.Validate(); err != nil {
		return Seed{}, err
	}
	return seed, nil
}

type Registry struct{ sources map[string]Source }

func NewRegistry() *Registry { return &Registry{sources: map[string]Source{}} }

// DefaultRegistry has the sources that need no credentials.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("manual", Manual{})
	return r
}

// Register adds a source under a `source.type` name. Registering nothing, nothing
// under a name, or the same name twice is a programming error.
func (r *Registry) Register(typ string, s Source) {
	if typ == "" || s == nil {
		panic("ticket: Register needs a type and a source")
	}
	if _, dup := r.sources[typ]; dup {
		panic(fmt.Sprintf("ticket: source %q registered twice", typ))
	}
	r.sources[typ] = s
}

func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.sources))
	for t := range r.sources {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// Fetch asks the source of the given type, then makes the seed usable: its id is
// sanitized (a Jira key becomes a ticket id) and the result must be a valid seed.
func (r *Registry) Fetch(ctx context.Context, typ, ref string) (Seed, error) {
	src, ok := r.sources[typ]
	if !ok {
		return Seed{}, fmt.Errorf("%w %q (known: %s)", ErrUnknownSource, typ, strings.Join(r.Types(), ", "))
	}
	seed, err := src.Fetch(ctx, ref)
	if err != nil {
		return Seed{}, fmt.Errorf("fetch %s %q: %w", typ, ref, err)
	}
	seed.ID = SanitizeID(seed.ID)
	if err := seed.Validate(); err != nil {
		return Seed{}, fmt.Errorf("%w: %s %q: %v", ErrUnusable, typ, ref, err)
	}
	return seed, nil
}
