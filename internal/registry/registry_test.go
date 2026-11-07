package registry_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t   *testing.T
	dir string
	mgr *state.Manager
	reg *registry.Registry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tickets")
	mgr := state.NewManager(testkit.NewFakeClock(epoch))
	return &fixture{t: t, dir: dir, mgr: mgr, reg: registry.New(dir, mgr)}
}

func (f *fixture) ticketDir(id string) string {
	f.t.Helper()
	dir := filepath.Join(f.dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *fixture) writeYAML(id, doc string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.ticketDir(id), "ticket.yaml"), []byte(doc), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeState(id string, s state.State) {
	f.t.Helper()
	s.TicketID = id
	if err := f.mgr.Create(filepath.Join(f.ticketDir(id), "state.json"), s); err != nil {
		f.t.Fatal(err)
	}
}

func yaml(id, title string, services ...string) string {
	doc := "id: " + id + "\ntitle: " + title + "\nservices:\n"
	for _, s := range services {
		doc += "  - {name: " + s + ", repo: /repos/" + s + ", branch: feat/" + id + "}\n"
	}
	return doc
}

func (f *fixture) list() []api.TicketSummary {
	f.t.Helper()
	got, err := f.reg.List()
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestListWithNoTickets(t *testing.T) {
	f := newFixture(t)
	if got := f.list(); len(got) != 0 {
		t.Errorf("tickets = %v, want none (the directory does not exist yet)", got)
	}
	f.ticketDir(".hidden")
	os.WriteFile(filepath.Join(f.dir, "notes.txt"), []byte("x"), 0o600)
	if got := f.list(); len(got) != 0 {
		t.Errorf("tickets = %v; hidden directories and stray files are not tickets", got)
	}
}

func TestListSummarizesTicketsInIDOrder(t *testing.T) {
	f := newFixture(t)
	f.writeYAML("zeta", yaml("zeta", "Last ticket", "api"))
	f.writeState("zeta", state.State{})
	f.writeYAML("alpha", yaml("alpha", "First ticket", "backend", "frontend"))
	f.writeState("alpha", state.State{
		Phase:    domain.PhaseImplement,
		Contract: state.Contract{Pending: true},
		Services: map[string]state.ServiceState{
			"backend":  {Status: domain.ServiceRunning, HostPort: 41000, Agent: state.Agent{State: domain.AgentWorking}},
			"frontend": {Status: domain.ServiceProvisioned},
		},
	})

	got := f.list()
	if len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "zeta" {
		t.Fatalf("order = %v, want alpha then zeta", got)
	}
	alpha := got[0]
	if alpha.Title != "First ticket" || alpha.Phase != domain.PhaseImplement || !alpha.ContractPending || alpha.Error != "" {
		t.Errorf("alpha = %+v", alpha)
	}
	if len(alpha.Services) != 2 {
		t.Fatalf("services = %+v", alpha.Services)
	}
	backend, frontend := alpha.Services[0], alpha.Services[1]
	if backend.Name != "backend" || backend.Status != domain.ServiceRunning || backend.AgentState != domain.AgentWorking || backend.HostPort != 41000 {
		t.Errorf("backend = %+v", backend)
	}
	if frontend.Name != "frontend" || frontend.Status != domain.ServiceProvisioned || frontend.HostPort != 0 {
		t.Errorf("frontend = %+v", frontend)
	}
	if zeta := got[1]; zeta.Phase != domain.PhasePlan || zeta.Services[0].Status != "" {
		t.Errorf("zeta (a new ticket with no service state yet) = %+v", zeta)
	}
}

func TestBrokenTicketsAreListedWithAnError(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture)
		want  string
	}{
		{"malformed ticket.yaml", func(f *fixture) {
			f.writeYAML("broken", "id: broken\nservices: [not, a, mapping\n")
			f.writeState("broken", state.State{Phase: domain.PhaseTest})
		}, "ticket.yaml"},
		{"invalid ticket.yaml", func(f *fixture) {
			f.writeYAML("broken", "id: broken\ntitle: T\n")
			f.writeState("broken", state.State{})
		}, "services"},
		{"missing state.json", func(f *fixture) {
			f.writeYAML("broken", yaml("broken", "T", "api"))
		}, "state.json"},
		{"corrupt state.json", func(f *fixture) {
			f.writeYAML("broken", yaml("broken", "T", "api"))
			os.WriteFile(filepath.Join(f.dir, "broken", "state.json"), []byte("{nope"), 0o600)
		}, "state.json"},
		{"no ticket.yaml at all", func(f *fixture) {
			f.ticketDir("broken")
		}, "ticket.yaml"},
		{"ticket.yaml names a different id", func(f *fixture) {
			f.writeYAML("broken", yaml("other", "T", "api"))
			f.writeState("broken", state.State{})
		}, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.writeYAML("fine", yaml("fine", "Healthy", "api"))
			f.writeState("fine", state.State{})
			tt.setup(f)

			got := f.list()
			if len(got) != 2 || got[0].ID != "broken" || got[1].ID != "fine" {
				t.Fatalf("tickets = %v; a broken ticket must be listed, not hidden", got)
			}
			if !strings.Contains(got[0].Error, tt.want) {
				t.Errorf("error = %q, want it to mention %q", got[0].Error, tt.want)
			}
			if got[1].Error != "" {
				t.Errorf("the healthy ticket picked up an error: %q", got[1].Error)
			}
		})
	}
}

func TestBrokenTicketKeepsWhatItCanStillShow(t *testing.T) {
	f := newFixture(t)
	f.writeYAML("a", yaml("a", "Has state only", "api")[:10]+"\n bad: [")
	f.writeState("a", state.State{Phase: domain.PhaseReview})
	got := f.list()[0]
	if got.Phase != domain.PhaseReview || got.Error == "" {
		t.Errorf("summary = %+v; the readable state.json should still show its phase", got)
	}

	f2 := newFixture(t)
	f2.writeYAML("b", yaml("b", "Has yaml only", "api", "web"))
	got = f2.list()[0]
	if got.Title != "Has yaml only" || len(got.Services) != 2 || got.Error == "" {
		t.Errorf("summary = %+v; the readable ticket.yaml should still show its title and services", got)
	}
}

func TestAMissingRepositoryDoesNotBreakTheListing(t *testing.T) {
	f := newFixture(t)
	f.writeYAML("gone", yaml("gone", "Repo moved", "api")) // /repos/api does not exist
	f.writeState("gone", state.State{})
	if got := f.list()[0]; got.Error != "" {
		t.Errorf("error = %q; listing must not validate repository paths (starting a ticket does)", got.Error)
	}
}

func TestListSeesTicketsAddedAndRemovedOnDisk(t *testing.T) {
	f := newFixture(t)
	f.writeYAML("one", yaml("one", "One", "api"))
	f.writeState("one", state.State{})
	if n := len(f.list()); n != 1 {
		t.Fatalf("tickets = %d, want 1", n)
	}

	f.writeYAML("two", yaml("two", "Two", "api"))
	f.writeState("two", state.State{})
	if n := len(f.list()); n != 2 {
		t.Errorf("a ticket created by hand did not show up: %d tickets", n)
	}

	os.RemoveAll(filepath.Join(f.dir, "one"))
	got := f.list()
	if len(got) != 1 || got[0].ID != "two" {
		t.Errorf("a removed ticket still listed: %v", got)
	}
}

func TestGet(t *testing.T) {
	f := newFixture(t)
	f.writeYAML("alpha", yaml("alpha", "Alpha", "backend"))
	f.writeState("alpha", state.State{Phase: domain.PhaseTest})

	t.Run("a healthy ticket", func(t *testing.T) {
		got, err := f.reg.Get("alpha")
		if err != nil {
			t.Fatal(err)
		}
		if got.Summary.ID != "alpha" || got.Ticket == nil || got.Ticket.Services[0].Name != "backend" {
			t.Errorf("ticket part = %+v", got)
		}
		if got.State == nil || got.State.Phase != domain.PhaseTest {
			t.Errorf("state part = %+v", got.State)
		}
	})

	t.Run("a ticket that does not exist", func(t *testing.T) {
		if _, err := f.reg.Get("nope"); !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("an id that could escape the tickets directory", func(t *testing.T) {
		if _, err := f.reg.Get("../alpha"); err == nil || errors.Is(err, registry.ErrNotFound) {
			t.Errorf("error = %v, want a validation error", err)
		}
	})

	t.Run("a broken ticket returns what could be read", func(t *testing.T) {
		f.writeYAML("sick", "id: sick\nservices: [")
		f.writeState("sick", state.State{Phase: domain.PhasePush})
		got, err := f.reg.Get("sick")
		if err != nil {
			t.Fatal(err)
		}
		if got.Summary.Error == "" || got.Ticket != nil || got.State == nil || got.State.Phase != domain.PhasePush {
			t.Errorf("get of a broken ticket = %+v", got)
		}
	})
}
