package state_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

const formatsExample = `{
  "schema_version": 1,
  "ticket_id": "ticket-412",
  "phase": "plan",
  "approvals": [
    {"gate": "plan_approval", "service": "", "by": "human", "at": "2026-11-05T14:03:00Z"}
  ],
  "transitions": [
    {"from": "plan", "to": "implement", "by": "human", "reason": "", "at": "2026-11-05T14:05:00Z"}
  ],
  "services": {
    "backend": {
      "status": "pending",
      "checkout": {"kind": "clone", "path": "/abs/path"},
      "branch": "feat/x",
      "base": "main",
      "host_port": 0,
      "container_id": "",
      "agent": {"backend": "herdr", "pane_id": "", "state": "unknown", "container_id": ""},
      "push": {"status": "pending", "pr_url": "", "error": ""},
      "error": ""
    }
  },
  "dependencies": {"db": {"container_id": "", "status": "pending"}},
  "network": {"name": "", "id": ""},
  "contract": {"pending": false},
  "manual_tests": {"steps": {}, "signed_off": false},
  "created_at": "2026-11-05T14:00:00Z",
  "updated_at": "2026-11-05T14:05:00Z"
}`

func newManager() (*state.Manager, *testkit.FakeClock) {
	c := testkit.NewFakeClock(epoch)
	return state.NewManager(c), c
}

func write(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("state file is not valid JSON: %v\n%s", err, raw)
	}
	return m
}

func TestLoadReadsTheFormatsExample(t *testing.T) {
	m, _ := newManager()
	got, err := m.Load(write(t, formatsExample))
	if err != nil {
		t.Fatal(err)
	}
	backend := got.Services["backend"]
	checks := []struct {
		name      string
		got, want any
	}{
		{"ticket", got.TicketID, "ticket-412"},
		{"phase", got.Phase, domain.PhasePlan},
		{"approval gate", got.Approvals[0].Gate, domain.GatePlanApproval},
		{"approval by", got.Approvals[0].By, "human"},
		{"transition", got.Transitions[0].To, domain.PhaseImplement},
		{"checkout kind", backend.Checkout.Kind, domain.CheckoutClone},
		{"checkout path", backend.Checkout.Path, "/abs/path"},
		{"service status", backend.Status, domain.ServicePending},
		{"agent backend", backend.Agent.Backend, "herdr"},
		{"agent state", backend.Agent.State, domain.AgentUnknown},
		{"push status", backend.Push.Status, domain.PushPending},
		{"dependency status", got.Dependencies["db"].Status, domain.ServicePending},
		{"created", got.CreatedAt, time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestRewritingTheExampleKeepsItsContent(t *testing.T) {
	m, _ := newManager()
	path := write(t, formatsExample)
	if err := m.Update(path, func(*state.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	json.Unmarshal([]byte(formatsExample), &want)
	got := readMap(t, path)
	want["updated_at"] = got["updated_at"] // an update stamps the time
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rewrite changed the document:\n got %v\nwant %v", got, want)
	}
}

func TestCreate(t *testing.T) {
	m, clock := newManager()
	path := filepath.Join(t.TempDir(), "state.json")

	if err := m.Create(path, state.State{TicketID: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := m.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.TicketID != "ticket-1" || got.Phase != domain.PhasePlan || got.SchemaVersion != 1 {
		t.Errorf("initial state = %+v", got)
	}
	if !got.CreatedAt.Equal(epoch) || !got.UpdatedAt.Equal(epoch) {
		t.Errorf("timestamps = %v / %v, want the clock's %v", got.CreatedAt, got.UpdatedAt, epoch)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}

	clock.Advance(time.Hour)
	if err := m.Create(path, state.State{TicketID: "ticket-1"}); err == nil {
		t.Error("creating over an existing state file must fail")
	}
	if err := m.Create(filepath.Join(t.TempDir(), "state.json"), state.State{TicketID: "Bad Id"}); err == nil {
		t.Error("an invalid ticket id must be rejected")
	}
}

func TestUpdate(t *testing.T) {
	m, clock := newManager()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Create(path, state.State{TicketID: "ticket-1"}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(time.Hour)
	err := m.Update(path, func(s *state.State) error {
		s.Phase = domain.PhaseImplement
		s.Services = map[string]state.ServiceState{"api": {Branch: "feat/x"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m.Load(path)
	if got.Phase != domain.PhaseImplement || got.Services["api"].Branch != "feat/x" {
		t.Errorf("update not applied: %+v", got)
	}
	if !got.CreatedAt.Equal(epoch) || !got.UpdatedAt.Equal(epoch.Add(time.Hour)) {
		t.Errorf("created/updated = %v / %v", got.CreatedAt, got.UpdatedAt)
	}

	before, _ := os.ReadFile(path)
	boom := errors.New("boom")
	err = m.Update(path, func(s *state.State) error {
		s.Phase = domain.PhaseDone
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a failed update changed the file")
	}
}

func TestEmptyFieldsAreWrittenAsTheirDefaults(t *testing.T) {
	m, _ := newManager()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Create(path, state.State{
		TicketID:     "ticket-1",
		Services:     map[string]state.ServiceState{"api": {}},
		Dependencies: map[string]state.DependencyState{"db": {}},
	}); err != nil {
		t.Fatal(err)
	}
	doc := readMap(t, path)
	for _, key := range []string{"approvals", "transitions"} {
		if list, ok := doc[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %#v, want an empty list (not null)", key, doc[key])
		}
	}
	steps := doc["manual_tests"].(map[string]any)["steps"]
	if m, ok := steps.(map[string]any); !ok || len(m) != 0 {
		t.Errorf("manual_tests.steps = %#v, want an empty object (not null)", steps)
	}
	got, _ := m.Load(path)
	api := got.Services["api"]
	if api.Status != domain.ServicePending || api.Agent.State != domain.AgentUnknown || api.Push.Status != domain.PushPending {
		t.Errorf("service defaults = %+v", api)
	}
	if got.Dependencies["db"].Status != domain.ServicePending {
		t.Errorf("dependency default = %+v", got.Dependencies["db"])
	}
}

func TestConcurrentUpdatesAreNotLost(t *testing.T) {
	m, _ := newManager()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.Create(path, state.State{TicketID: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	const workers = 25
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := m.Update(path, func(s *state.State) error {
				s.Approvals = append(s.Approvals, state.Approval{Gate: domain.GatePlanApproval, By: "human"})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := m.Load(path)
	if err != nil || len(got.Approvals) != workers {
		t.Fatalf("approvals = %d, %v; want %d (lost updates)", len(got.Approvals), err, workers)
	}
}

func TestUnknownFieldsSurviveARewrite(t *testing.T) {
	doc := strings.Replace(formatsExample, `"phase": "plan",`, `"phase": "plan", "future_top_level": {"x": [1, 2]},`, 1)
	doc = strings.Replace(doc, `"status": "pending",
      "checkout"`, `"status": "pending", "future_service_field": 7,
      "checkout"`, 1)
	m, _ := newManager()
	path := write(t, doc)
	if err := m.Update(path, func(s *state.State) error {
		s.Phase = domain.PhaseImplement
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := readMap(t, path)
	if got["phase"] != "implement" {
		t.Errorf("update lost: phase = %v", got["phase"])
	}
	if !reflect.DeepEqual(got["future_top_level"], map[string]any{"x": []any{1.0, 2.0}}) {
		t.Errorf("unknown top-level field not preserved: %v", got["future_top_level"])
	}
	backend := got["services"].(map[string]any)["backend"].(map[string]any)
	if backend["future_service_field"] != 7.0 {
		t.Errorf("unknown service field not preserved: %v", backend["future_service_field"])
	}
}

func TestMissingFileIsNotFound(t *testing.T) {
	m, _ := newManager()
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := m.Load(path); !errors.Is(err, state.ErrNotFound) || !strings.Contains(err.Error(), path) {
		t.Errorf("Load error = %v, want ErrNotFound naming the path", err)
	}
	if err := m.Update(path, func(*state.State) error { return nil }); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Update error = %v, want ErrNotFound", err)
	}
}

func TestBadFilesErrorClearly(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"older schema", strings.Replace(formatsExample, `"schema_version": 1`, `"schema_version": 0`, 1), "older"},
		{"newer schema", strings.Replace(formatsExample, `"schema_version": 1`, `"schema_version": 2`, 1), "newer"},
		{"corrupt json", `{"schema_version": 1, "phase": `, "state.json"},
		{"unknown phase", strings.Replace(formatsExample, `"phase": "plan"`, `"phase": "daydream"`, 1), "daydream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newManager()
			_, err := m.Load(write(t, tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

func TestLeftoverTempFilesAreIgnored(t *testing.T) {
	m, _ := newManager()
	path := write(t, formatsExample)
	if err := os.WriteFile(path+".tmp-crashed", []byte(`{"schema_version": 1, "pha`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Load(path); err != nil || got.TicketID != "ticket-412" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if err := m.Update(path, func(*state.State) error { return nil }); err != nil {
		t.Errorf("Update broke after a crash left a temp file: %v", err)
	}
}
