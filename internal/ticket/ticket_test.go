package ticket_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

var ctx = context.Background()

func TestSanitizeIDProducesAValidTicketID(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"PROJ-412", "proj-412"},
		{"Fix the login bug!", "fix-the-login-bug"},
		{"  --weird__id..  ", "weird-id"},
		{"owner/repo#123", "owner-repo-123"},
		{"Café ñ 7", "caf-7"},
		{"already-fine-1", "already-fine-1"},
		{strings.Repeat("a", 80), strings.Repeat("a", 63)},
		{strings.Repeat("a", 62) + " b", strings.Repeat("a", 62)},
		{"!!!", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := ticket.SanitizeID(tt.in)
			if got != tt.want {
				t.Errorf("SanitizeID(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got != "" {
				if err := domain.ValidateTicketID(got); err != nil {
					t.Errorf("the result %q is not a valid ticket id: %v", got, err)
				}
			}
		})
	}
}

func TestSeedValidation(t *testing.T) {
	tests := []struct {
		name    string
		seed    ticket.Seed
		wantErr string
	}{
		{"valid", ticket.Seed{ID: "demo", Title: "Demo"}, ""},
		{"missing title", ticket.Seed{ID: "demo"}, "title"},
		{"invalid id", ticket.Seed{ID: "Demo One", Title: "Demo"}, "ticket id"},
		{"missing id", ticket.Seed{Title: "Demo"}, "ticket id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.seed.Validate()
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("Validate = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestManualSource(t *testing.T) {
	t.Run("a seed straight from flags", func(t *testing.T) {
		got, err := ticket.Manual{ID: "demo", Title: "A demo", Description: "Some context"}.Fetch(ctx, "")
		want := ticket.Seed{ID: "demo", Title: "A demo", Description: "Some context"}
		if err != nil || got != want {
			t.Errorf("Fetch = %+v, %v; want %+v", got, err, want)
		}
	})

	t.Run("an id typed by the user is checked, not silently rewritten", func(t *testing.T) {
		if _, err := (ticket.Manual{ID: "Demo Ticket", Title: "x"}).Fetch(ctx, ""); err == nil {
			t.Error("expected an error for an invalid explicit id")
		}
	})

	t.Run("a bare reference becomes the title and a sanitized id", func(t *testing.T) {
		got, err := ticket.Manual{}.Fetch(ctx, "Fix the login bug")
		if err != nil || got.ID != "fix-the-login-bug" || got.Title != "Fix the login bug" {
			t.Errorf("Fetch = %+v, %v", got, err)
		}
	})

	t.Run("explicit flags win over the reference", func(t *testing.T) {
		got, err := ticket.Manual{ID: "chosen"}.Fetch(ctx, "Some title")
		if err != nil || got.ID != "chosen" || got.Title != "Some title" {
			t.Errorf("Fetch = %+v, %v", got, err)
		}
	})

	t.Run("nothing to go on", func(t *testing.T) {
		if _, err := (ticket.Manual{}).Fetch(ctx, ""); err == nil {
			t.Error("a manual ticket needs at least an id or a title")
		}
		if _, err := (ticket.Manual{}).Fetch(ctx, "!!!"); err == nil || !strings.Contains(err.Error(), "!!!") {
			t.Errorf("error = %v, want one that cannot derive an id from the reference", err)
		}
	})
}

type stubSource struct {
	seed ticket.Seed
	err  error
	got  string
}

func (s *stubSource) Fetch(_ context.Context, ref string) (ticket.Seed, error) {
	s.got = ref
	return s.seed, s.err
}

func TestRegistry(t *testing.T) {
	t.Run("the default registry knows the manual source", func(t *testing.T) {
		r := ticket.DefaultRegistry()
		seed, err := r.Fetch(ctx, "manual", "Plan the migration")
		if err != nil || seed.ID != "plan-the-migration" {
			t.Errorf("Fetch = %+v, %v", seed, err)
		}
		if types := r.Types(); len(types) != 1 || types[0] != "manual" {
			t.Errorf("Types = %v", types)
		}
	})

	t.Run("an unknown type names the known ones", func(t *testing.T) {
		r := ticket.DefaultRegistry()
		r.Register("zeta", &stubSource{})
		r.Register("alpha", &stubSource{})
		_, err := r.Fetch(ctx, "jira", "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "jira") || !strings.Contains(err.Error(), "alpha, manual, zeta") || !errors.Is(err, ticket.ErrUnknownSource) {
			t.Errorf("error = %v, want ErrUnknownSource naming jira and the known types, sorted", err)
		}
	})

	t.Run("the source receives the reference and its id is sanitized", func(t *testing.T) {
		src := &stubSource{seed: ticket.Seed{ID: "PROJ-412", Title: "Dashboard", Description: "d", URL: "https://example.test/PROJ-412"}}
		r := ticket.NewRegistry()
		r.Register("jira", src)
		seed, err := r.Fetch(ctx, "jira", "PROJ-412")
		if err != nil || src.got != "PROJ-412" {
			t.Fatalf("Fetch = %+v, %v (source saw %q)", seed, err, src.got)
		}
		if seed.ID != "proj-412" || seed.URL != "https://example.test/PROJ-412" || seed.Title != "Dashboard" {
			t.Errorf("seed = %+v", seed)
		}
	})

	t.Run("a source's failure is passed on", func(t *testing.T) {
		r := ticket.NewRegistry()
		r.Register("jira", &stubSource{err: ticket.ErrNotFound})
		if _, err := r.Fetch(ctx, "jira", "PROJ-9"); !errors.Is(err, ticket.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("a seed that cannot become a ticket is rejected", func(t *testing.T) {
		r := ticket.NewRegistry()
		r.Register("jira", &stubSource{seed: ticket.Seed{ID: "!!!", Title: "x"}})
		if _, err := r.Fetch(ctx, "jira", "x"); !errors.Is(err, ticket.ErrUnusable) {
			t.Errorf("error = %v, want ErrUnusable for an id that sanitizes to nothing", err)
		}
		r.Register("github", &stubSource{seed: ticket.Seed{ID: "gh-1"}})
		if _, err := r.Fetch(ctx, "github", "x"); err == nil || !strings.Contains(err.Error(), "title") {
			t.Errorf("error = %v, want one about the missing title", err)
		}
	})

	t.Run("registering badly is a programming error", func(t *testing.T) {
		for name, fn := range map[string]func(){
			"empty type": func() { ticket.NewRegistry().Register("", &stubSource{}) },
			"nil source": func() { ticket.NewRegistry().Register("x", nil) },
			"duplicate":  func() { r := ticket.NewRegistry(); r.Register("x", &stubSource{}); r.Register("x", &stubSource{}) },
		} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s did not panic", name)
					}
				}()
				fn()
			}()
		}
	})
}
