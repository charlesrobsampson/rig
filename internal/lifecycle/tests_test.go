package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func writeManualTests(t *testing.T, f *fixture, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "demo", "manual-tests.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRenderTestsFillsLiveAddressesAndStepProgress(t *testing.T) {
	f, dk := logsFixture(t, nil)
	_ = dk
	started := f.state("demo").Services["svc1"].HostPort
	writeManualTests(t, f, strings.Join([]string{
		"# Tests", "Ticket {{ticket.id}}", "",
		"### health: health responds", "curl {{svc1.url}}/healthz   # port {{svc1.port}}", "",
		"### inside: reachable from inside", "{{svc1.internal_url}} and {{db.host}}:5432",
	}, "\n"))
	f.states.Update(filepath.Join(f.dir, "demo", "state.json"), func(s *state.State) error {
		s.ManualTests.Steps = map[string]bool{"health": true}
		return nil
	})

	got, err := f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Preamble != "# Tests\nTicket demo" || len(got.Steps) != 2 || got.SignedOff {
		t.Fatalf("result = %+v", got)
	}
	if want := "curl http://localhost:" + itoa(started) + "/healthz   # port " + itoa(started); got.Steps[0].Body != want || !got.Steps[0].Done {
		t.Errorf("health step = %+v, want body %q and done", got.Steps[0], want)
	}
	if got.Steps[1].Body != "http://svc1:8080 and db:5432" || got.Steps[1].Done {
		t.Errorf("inside step = %+v", got.Steps[1])
	}
}

func TestRenderTestsOfAStoppedTicketExplainsWhichServiceIsDown(t *testing.T) {
	f, _ := logsFixture(t, nil)
	f.mustStop("demo", false, false)
	writeManualTests(t, f, "### health: up?\ncurl {{svc1.url}}/healthz\n")
	_, err := f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "demo"})
	var apiErr *api.Error
	if apiCode(t, err) != api.CodeInvalid {
		t.Fatalf("error = %v", err)
	}
	apiErr = err.(*api.Error)
	if len(apiErr.Details) != 1 || !strings.Contains(apiErr.Details[0], "service svc1 has no host port right now (stopped)") {
		t.Errorf("details = %v", apiErr.Details)
	}
}

func TestRenderTestsListsEveryProblemAndHandlesAMissingFile(t *testing.T) {
	f, _ := logsFixture(t, nil)
	writeManualTests(t, f, "### a: x\n{{nope.url}} {{svc1.bogus}}\n### Bad: y\n")
	_, err := f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "demo"})
	if apiCode(t, err) != api.CodeInvalid {
		t.Fatalf("error = %v", err)
	}

	writeManualTests(t, f, "### a: x\n{{nope.url}} {{svc1.bogus}}\n")
	_, err = f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "demo"})
	if details := err.(*api.Error).Details; len(details) != 2 {
		t.Errorf("details = %v, want both unknown placeholders", details)
	}

	os.Remove(filepath.Join(f.dir, "demo", "manual-tests.md"))
	if _, err := f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "demo"}); apiCode(t, err) != api.CodeNotFound || !strings.Contains(err.Error(), "test phase") {
		t.Errorf("a missing file = %v", err)
	}
	if _, err := f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "ghost"}); apiCode(t, err) != api.CodeNotFound {
		t.Errorf("an unknown ticket = %v", err)
	}
}

func TestRenderTestsWithoutAnyPlaceholderPassesThrough(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t))
	writeManualTests(t, f, "### manual: do it by hand\n\nLook at the screen. Use {braces} freely.\n")
	got, err := f.lc.RenderTests(ctx, api.TestsRenderParams{Ticket: "demo"})
	if err != nil || len(got.Steps) != 1 || got.Steps[0].Body != "Look at the screen. Use {braces} freely." {
		t.Errorf("result = %+v, %v", got, err)
	}
}
