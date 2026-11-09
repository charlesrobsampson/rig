package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

var ctx = context.Background()

type creatorFixture struct {
	dir     string
	sources *ticket.Registry
	fake    *testkit.FakeTicketSource
	creator *registry.Creator
	mgr     *state.Manager
}

func newCreator(t *testing.T) *creatorFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tickets")
	mgr := state.NewManager(testkit.NewFakeClock(epoch))
	fake := testkit.NewFakeTicketSource()
	sources := ticket.DefaultRegistry()
	sources.Register("fake", fake)
	return &creatorFixture{
		dir: dir, sources: sources, fake: fake, mgr: mgr,
		creator: registry.NewCreator(registry.CreatorOptions{
			Dir:          dir,
			States:       mgr,
			Sources:      sources,
			RepoCheck:    func(string) error { return nil },
			BranchPrefix: "rig/",
		}),
	}
}

func svc(name string) api.ServiceSpec { return api.ServiceSpec{Name: name, Repo: "/repos/" + name} }

func asAPIError(t *testing.T, err error) *api.Error {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an *api.Error", err)
	}
	return apiErr
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCreateWritesTheTicketFiles(t *testing.T) {
	f := newCreator(t)
	res, err := f.creator.Create(ctx, api.TicketNewParams{ID: "demo", Title: "Demo ticket", Description: "Some context.", Services: []api.ServiceSpec{svc("api")}})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.dir, "demo")
	if res.Path != want || res.Summary.ID != "demo" || res.Summary.Phase != domain.PhasePlan || res.Summary.Error != "" {
		t.Fatalf("result = %+v", res)
	}

	for _, name := range []string{"ticket.yaml", "state.json", "plan.md", "tasks.md", "manual-tests.md"} {
		info, err := os.Stat(filepath.Join(want, name))
		if err != nil {
			t.Errorf("%s is missing: %v", name, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
	if info, _ := os.Stat(want); info.Mode().Perm() != 0o700 {
		t.Errorf("ticket directory mode = %v, want 0700", info.Mode().Perm())
	}

	plan := read(t, filepath.Join(want, "plan.md"))
	for _, s := range []string{"# Demo ticket", "Some context.", "## Goal", "## Approach"} {
		if !strings.Contains(plan, s) {
			t.Errorf("plan.md lacks %q:\n%s", s, plan)
		}
	}
	if tasks := read(t, filepath.Join(want, "tasks.md")); !strings.Contains(tasks, "- [ ]") {
		t.Errorf("tasks.md has no checkbox:\n%s", tasks)
	}
	if manual := read(t, filepath.Join(want, "manual-tests.md")); !strings.Contains(manual, "{{api.url}}") || !strings.Contains(manual, "### ") {
		t.Errorf("manual-tests.md should show the step and placeholder format using the first service:\n%s", manual)
	}
}

func TestTheGeneratedFilesAreValid(t *testing.T) {
	f := newCreator(t)
	_, err := f.creator.Create(ctx, api.TicketNewParams{
		ID: "demo", Title: `A "tricky": title # with yaml characters`,
		Services: []api.ServiceSpec{svc("api"), {Name: "web", Repo: "/repos/web", Branch: "feat/custom"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, "demo")

	got, err := config.LoadTicket(filepath.Join(dir, "ticket.yaml"), config.TicketOptions{RepoCheck: func(string) error { return nil }})
	if err != nil {
		t.Fatalf("the generated ticket.yaml does not pass the loader: %v", err)
	}
	if got.ID != "demo" || got.Title != `A "tricky": title # with yaml characters` || got.Source.Type != "manual" {
		t.Errorf("ticket = %+v", got)
	}
	if len(got.Services) != 2 || got.Services[0].Name != "api" || got.Services[0].Branch != "rig/demo/api" || got.Services[1].Branch != "feat/custom" {
		t.Errorf("services = %+v; want the default branch rig/<ticket>/<service> and the explicit one kept", got.Services)
	}
	yamlText := read(t, filepath.Join(dir, "ticket.yaml"))
	for _, noise := range []string{"gates", "docker", "policy", "egress", "build"} {
		if strings.Contains(yamlText, noise) {
			t.Errorf("a new ticket.yaml should hold only what was asked for, not %q:\n%s", noise, yamlText)
		}
	}
	if got.Services[0].Policy.Gates != nil {
		t.Errorf("a new ticket must not pin gates; they are inherited: %#v", got.Services[0].Policy.Gates)
	}

	st, err := f.mgr.Load(filepath.Join(dir, "state.json"))
	if err != nil || st.Phase != domain.PhasePlan || st.TicketID != "demo" {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if len(st.Services) != 2 || st.Services["api"].Branch != "rig/demo/api" || st.Services["web"].Status != domain.ServicePending {
		t.Errorf("initial service state = %+v", st.Services)
	}
}

func TestACreatedTicketShowsUpInTheRegistry(t *testing.T) {
	f := newCreator(t)
	if _, err := f.creator.Create(ctx, api.TicketNewParams{ID: "demo", Title: "Demo", Services: []api.ServiceSpec{svc("api")}}); err != nil {
		t.Fatal(err)
	}
	list, err := registry.New(f.dir, f.mgr).List()
	if err != nil || len(list) != 1 || list[0].ID != "demo" || list[0].Phase != domain.PhasePlan || list[0].Error != "" {
		t.Errorf("list = %+v, %v", list, err)
	}
}

func TestCreatingAnExistingTicketChangesNothing(t *testing.T) {
	f := newCreator(t)
	params := api.TicketNewParams{ID: "demo", Title: "Demo", Services: []api.ServiceSpec{svc("api")}}
	if _, err := f.creator.Create(ctx, params); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, "demo")
	os.WriteFile(filepath.Join(dir, "plan.md"), []byte("my careful edits"), 0o600)
	os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o600)

	params.Title = "A different title"
	_, err := f.creator.Create(ctx, params)
	if e := asAPIError(t, err); e.Code != api.CodeConflict || !strings.Contains(e.Message, "demo") {
		t.Fatalf("error = %v, want a conflict naming the ticket", err)
	}
	if got := read(t, filepath.Join(dir, "plan.md")); got != "my careful edits" {
		t.Errorf("plan.md was overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.txt")); err != nil {
		t.Error("an existing file was removed")
	}
	if yaml := read(t, filepath.Join(dir, "ticket.yaml")); strings.Contains(yaml, "A different title") {
		t.Error("ticket.yaml was rewritten")
	}
}

func TestConcurrentCreatesOfTheSameIDHaveExactlyOneWinner(t *testing.T) {
	f := newCreator(t)
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.creator.Create(ctx, api.TicketNewParams{ID: "race", Title: "Race", Services: []api.ServiceSpec{svc("api")}})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if asAPIError(t, err).Code != api.CodeConflict {
			t.Errorf("a loser got %v, want a conflict", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d creates succeeded, want exactly 1", wins)
	}
}

func TestCreateSeedsTheTicketFromASource(t *testing.T) {
	f := newCreator(t)
	f.fake.Add("PROJ-412", ticket.Seed{ID: "PROJ-412", Title: "Dashboard for users", Description: "As a user I want a dashboard.", URL: "https://tracker.example/PROJ-412"})

	t.Run("everything comes from the source", func(t *testing.T) {
		res, err := f.creator.Create(ctx, api.TicketNewParams{From: "fake:PROJ-412", Services: []api.ServiceSpec{svc("api")}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Summary.ID != "proj-412" || res.Summary.Title != "Dashboard for users" {
			t.Errorf("summary = %+v; the Jira-style id should be sanitized", res.Summary)
		}
		plan := read(t, filepath.Join(res.Path, "plan.md"))
		for _, s := range []string{"# Dashboard for users", "As a user I want a dashboard.", "https://tracker.example/PROJ-412"} {
			if !strings.Contains(plan, s) {
				t.Errorf("plan.md lacks %q:\n%s", s, plan)
			}
		}
		yaml := read(t, filepath.Join(res.Path, "ticket.yaml"))
		if !strings.Contains(yaml, "type: fake") || !strings.Contains(yaml, "key: PROJ-412") {
			t.Errorf("ticket.yaml does not record where the ticket came from:\n%s", yaml)
		}
	})

	t.Run("explicit values override the source", func(t *testing.T) {
		res, err := f.creator.Create(ctx, api.TicketNewParams{From: "fake:PROJ-412", ID: "my-id", Title: "My title", Description: "My notes", Services: []api.ServiceSpec{svc("api")}})
		if err != nil {
			t.Fatal(err)
		}
		plan := read(t, filepath.Join(res.Path, "plan.md"))
		if res.Summary.ID != "my-id" || res.Summary.Title != "My title" || !strings.Contains(plan, "My notes") || strings.Contains(plan, "As a user") {
			t.Errorf("summary = %+v\nplan:\n%s", res.Summary, plan)
		}
	})

	t.Run("a bare title is enough for a manual ticket", func(t *testing.T) {
		res, err := f.creator.Create(ctx, api.TicketNewParams{Title: "Fix the login bug", Services: []api.ServiceSpec{svc("api")}})
		if err != nil || res.Summary.ID != "fix-the-login-bug" {
			t.Errorf("result = %+v, %v", res, err)
		}
	})
}

func TestCreateReportsSourceProblems(t *testing.T) {
	f := newCreator(t)
	tests := []struct {
		name string
		from string
		prep func()
		code api.ErrorCode
		text string
	}{
		{"unknown source type", "jira:PROJ-1", nil, api.CodeInvalid, "known: fake, manual"},
		{"malformed reference", "no-colon", nil, api.CodeInvalid, "type:ref"},
		{"a reference the source does not have", "fake:PROJ-404", nil, api.CodeNotFound, "PROJ-404"},
		{"the source is down", "fake:PROJ-1", func() { f.fake.FailWith(errors.New("tracker unreachable")) }, api.CodeUnavailable, "tracker unreachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prep != nil {
				tt.prep()
			}
			_, err := f.creator.Create(ctx, api.TicketNewParams{From: tt.from, Services: []api.ServiceSpec{svc("api")}})
			if e := asAPIError(t, err); e.Code != tt.code || !strings.Contains(e.Message, tt.text) {
				t.Errorf("error = %v, want %s mentioning %q", err, tt.code, tt.text)
			}
		})
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
		t.Errorf("failed creates left directories behind: %v", entries)
	}
}

func TestCreateValidatesBeforeTouchingTheDisk(t *testing.T) {
	f := newCreator(t)
	f.creator = registry.NewCreator(registry.CreatorOptions{
		Dir: f.dir, States: f.mgr, Sources: f.sources, BranchPrefix: "rig/",
		RepoCheck: func(p string) error {
			if strings.Contains(p, "missing") {
				return errors.New("not a git repository")
			}
			return nil
		},
	})
	tests := []struct {
		name   string
		params api.TicketNewParams
		want   string
	}{
		{"no services", api.TicketNewParams{ID: "x", Title: "T"}, "service"},
		{"an invalid explicit id", api.TicketNewParams{ID: "Bad Id", Title: "T", Services: []api.ServiceSpec{svc("api")}}, "ticket id"},
		{"neither id nor title", api.TicketNewParams{Services: []api.ServiceSpec{svc("api")}}, "id or a title"},
		{"a bad service name", api.TicketNewParams{ID: "x", Title: "T", Services: []api.ServiceSpec{{Name: "API", Repo: "/repos/a"}}}, "API"},
		{"duplicate service names", api.TicketNewParams{ID: "x", Title: "T", Services: []api.ServiceSpec{svc("api"), svc("api")}}, "duplicate"},
		{"a relative repo", api.TicketNewParams{ID: "x", Title: "T", Services: []api.ServiceSpec{{Name: "api", Repo: "relative/repo"}}}, "absolute"},
		{"a repo that is not usable", api.TicketNewParams{ID: "x", Title: "T", Services: []api.ServiceSpec{{Name: "api", Repo: "/missing/repo"}}}, "not a git repository"},
		{"the same repo and branch twice", api.TicketNewParams{ID: "x", Title: "T", Services: []api.ServiceSpec{
			{Name: "a", Repo: "/repos/r", Branch: "feat/x"}, {Name: "b", Repo: "/repos/r", Branch: "feat/x"}}}, "branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.creator.Create(ctx, tt.params)
			if e := asAPIError(t, err); e.Code != api.CodeInvalid || !strings.Contains(e.Message, tt.want) {
				t.Errorf("error = %v, want invalid mentioning %q", err, tt.want)
			}
		})
	}
	if _, err := os.Stat(f.dir); err == nil {
		if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
			t.Errorf("rejected requests left files behind: %v", entries)
		}
	}
}

func TestTwoServicesFromOneRepoGetDistinctDefaultBranches(t *testing.T) {
	f := newCreator(t)
	_, err := f.creator.Create(ctx, api.TicketNewParams{ID: "demo", Title: "Demo", Services: []api.ServiceSpec{
		{Name: "api-one", Repo: "/repos/api"}, {Name: "api-two", Repo: "/repos/api"},
	}})
	if err != nil {
		t.Fatalf("two instances of one service are a supported setup: %v", err)
	}
}
