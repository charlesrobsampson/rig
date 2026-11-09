// Package secrets finds the values of the secrets a ticket names. Tickets and auth
// profiles list names only; the values come from the macOS Keychain or a .env file the
// user keeps outside rig, and are held in memory just long enough to be handed to a
// container. They are never written to the ticket directory, logs, or API payloads, and
// no error here ever contains one.
package secrets

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Source is one place secret values live.
type Source interface {
	// Lookup returns the value of name; found is false when this source does not have
	// it. An error means the source itself failed (a locked keychain, an unreadable file).
	Lookup(ctx context.Context, name string) (value string, found bool, err error)
	// Describe names the source for "not found" messages.
	Describe() string
}

// Resolver asks its sources in order; the first that has a name wins.
type Resolver struct {
	sources []Source
}

func New(sources ...Source) *Resolver { return &Resolver{sources: sources} }

// Resolve returns the value of every name. If any is missing it fails naming all of
// them and where it looked; it never returns a partial map.
func (r *Resolver) Resolve(ctx context.Context, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	var missing []string
	for _, name := range names {
		if _, done := out[name]; done {
			continue
		}
		value, found, err := r.lookup(ctx, name)
		if err != nil {
			return nil, err
		}
		if !found {
			missing = append(missing, name)
			continue
		}
		out[name] = value
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("secrets not found: %s (looked in %s)", strings.Join(missing, ", "), r.describe())
	}
	return out, nil
}

func (r *Resolver) lookup(ctx context.Context, name string) (string, bool, error) {
	for _, s := range r.sources {
		value, found, err := s.Lookup(ctx, name)
		if err != nil {
			return "", false, fmt.Errorf("look up secret %s in %s: %w", name, s.Describe(), err)
		}
		if found {
			return value, true, nil
		}
	}
	return "", false, nil
}

func (r *Resolver) describe() string {
	if len(r.sources) == 0 {
		return "no secret source is configured"
	}
	parts := make([]string, len(r.sources))
	for i, s := range r.sources {
		parts[i] = s.Describe()
	}
	return strings.Join(parts, ", then ")
}
