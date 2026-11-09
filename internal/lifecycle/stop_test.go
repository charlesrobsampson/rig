package lifecycle_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func (f *fixture) stop(id string, force, keep bool) (api.TicketStopResult, error) {
	return f.lc.Stop(ctx, api.TicketStopParams{ID: id, Force: force, KeepCheckouts: keep})
}

func (f *fixture) mustStop(id string, force, keep bool) api.TicketStopResult {
	f.t.Helper()
	res, err := f.stop(id, force, keep)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func commitAt(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.RunGit(t, dir, "add", file)
	testkit.RunGit(t, dir, "commit", "-q", "-m", "work on "+file)
	return testkit.RunGit(t, dir, "rev-parse", "HEAD")
}

func branchTip(repo *testkit.Repo, branch string) string {
	out, err := git.Trusted().Run(ctx, repo.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return out
}

func details(t *testing.T, err error) []string {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeBlocked {
		t.Fatalf("error = %v, want a blocked error", err)
	}
	return e.Details
}

func TestStopRemovesCheckoutsAndRecordsIt(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	started := f.mustStart("demo", "")
	f.events()

	res := f.mustStop("demo", false, false)
	for _, co := range started.Checkouts {
		if exists(co.Path) {
			t.Errorf("%s still exists after stop", co.Path)
		}
	}
	if len(res.Services) != 2 || !res.Services[0].Removed || !res.Services[1].Removed {
		t.Errorf("services = %+v", res.Services)
	}
	st := f.state("demo")
	for _, svc := range []string{"svc1", "svc2"} {
		if got := st.Services[svc]; got.Status != domain.ServiceStopped || got.Checkout.Path != "" || got.Checkout.Kind != "" {
			t.Errorf("state for %s = %+v", svc, got)
		}
	}
	if res.Summary.Services[0].Status != domain.ServiceStopped {
		t.Errorf("summary = %+v", res.Summary)
	}
	var types []string
	for _, ev := range f.events() {
		types = append(types, string(ev.Type)+"/"+ev.Service)
	}
	if want := "service.status/svc1,service.status/svc2,ticket.stopped/"; strings.Join(types, ",") != want {
		t.Errorf("events = %v, want %s", types, want)
	}
}

func TestStopImportsACloneBeforeDeletingIt(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	started := f.mustStart("demo", "")
	sha := commitAt(t, started.Checkouts[0].Path, "work.txt", "the agent's work\n")
	if branchTip(repo, "rig/demo/svc1") != "" {
		t.Fatal("the branch reached the main repo before stop")
	}

	res := f.mustStop("demo", false, false)
	if tip := branchTip(repo, "rig/demo/svc1"); tip != sha {
		t.Errorf("main repo's branch is at %q, want the clone's commit %q", tip, sha)
	}
	if !res.Services[0].Imported || !res.Services[0].Removed {
		t.Errorf("services = %+v", res.Services)
	}
	if exists(started.Checkouts[0].Path) {
		t.Error("the clone was not removed")
	}
}

func TestStopKeepsAWorktreesBranch(t *testing.T) {
	f := newFixture(t, domain.SandboxHost)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	started := f.mustStart("demo", "")
	sha := commitAt(t, started.Checkouts[0].Path, "work.txt", "work\n")

	res := f.mustStop("demo", false, false)
	if branchTip(repo, "rig/demo/svc1") != sha {
		t.Error("the worktree's branch lost its commit")
	}
	if res.Services[0].Imported || !res.Services[0].Removed {
		t.Errorf("services = %+v; a worktree has nothing to import", res.Services)
	}
	if out := repo.Git("worktree", "list"); strings.Contains(out, started.Checkouts[0].Path) {
		t.Errorf("the main repo still lists the worktree:\n%s", out)
	}
}

func TestUncommittedWorkBlocksTheWholeStopAndNamesTheService(t *testing.T) {
	tests := []struct {
		name   string
		dirty  func(t *testing.T, dir string)
		detail string
	}{
		{"a modified file", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o644)
		}, "1 modified file(s)"},
		{"an untracked file", func(t *testing.T, dir string) { os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("x"), 0o644) }, "1 untracked file(s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, domain.SandboxContainer)
			f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
			started := f.mustStart("demo", "")
			tt.dirty(t, started.Checkouts[1].Path) // only svc2 is dirty
			before := f.state("demo")
			f.events()

			_, err := f.stop("demo", false, false)
			d := details(t, err)
			if len(d) != 1 || !strings.HasPrefix(d[0], "svc2:") || !strings.Contains(d[0], tt.detail) {
				t.Errorf("details = %v, want exactly svc2's work listed (%s)", d, tt.detail)
			}
			for _, co := range started.Checkouts {
				if !exists(co.Path) {
					t.Errorf("%s was removed although the stop was refused: it must be all or nothing", co.Path)
				}
			}
			if after := f.state("demo"); after.Services["svc1"].Status != before.Services["svc1"].Status || after.Services["svc1"].Checkout.Path == "" {
				t.Errorf("a refused stop changed the state: %+v", after.Services["svc1"])
			}
			if evs := f.events(); len(evs) != 0 {
				t.Errorf("a refused stop published %d event(s)", len(evs))
			}
		})
	}
}

func TestForceDiscardsUncommittedWork(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t))
	started := f.mustStart("demo", "")
	os.WriteFile(filepath.Join(started.Checkouts[0].Path, "scratch.txt"), []byte("lost"), 0o644)

	if _, err := f.stop("demo", false, false); err == nil {
		t.Fatal("the unforced stop should have been refused")
	}
	f.mustStop("demo", true, false)
	if exists(started.Checkouts[0].Path) {
		t.Error("a forced stop left the checkout")
	}
}

func TestAnImportThatFailsBlocksTheStop(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	started := f.mustStart("demo", "")
	commitAt(t, started.Checkouts[0].Path, "clone.txt", "from the clone\n")
	// The main repo's branch moved on without the clone's commit.
	repo.Git("checkout", "-q", "-b", "rig/demo/svc1", "main")
	diverged := repo.Commit("main-side.txt", "x\n", "diverge")
	repo.Git("checkout", "-q", "main")

	_, err := f.stop("demo", false, false)
	d := details(t, err)
	if len(d) != 1 || !strings.HasPrefix(d[0], "svc1:") || !strings.Contains(d[0], "not imported") || !strings.Contains(d[0], "non-fast-forward") {
		t.Errorf("details = %v, want svc1's failed import explained", d)
	}
	if !exists(started.Checkouts[0].Path) {
		t.Error("the clone was removed although its commit could not be imported")
	}

	t.Run("force discards the commits but never overwrites the main branch", func(t *testing.T) {
		f.mustStop("demo", true, false)
		if exists(started.Checkouts[0].Path) {
			t.Error("the forced stop left the checkout")
		}
		if tip := branchTip(repo, "rig/demo/svc1"); tip != diverged {
			t.Errorf("a forced stop rewrote the main repo's branch: %q, want %q", tip, diverged)
		}
	})
}

func TestKeepCheckoutsImportsAndStopsButLeavesTheDirectories(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	started := f.mustStart("demo", "")
	sha := commitAt(t, started.Checkouts[0].Path, "work.txt", "x\n")
	os.WriteFile(filepath.Join(started.Checkouts[0].Path, "wip.txt"), []byte("uncommitted"), 0o644)

	res := f.mustStop("demo", false, true) // uncommitted work does not block: nothing is deleted
	if !exists(started.Checkouts[0].Path) || res.Services[0].Removed || !res.Services[0].Imported {
		t.Errorf("services = %+v; the directory must stay and the commits must still be imported", res.Services)
	}
	if branchTip(repo, "rig/demo/svc1") != sha {
		t.Error("the commits were not imported")
	}
	if got := f.state("demo").Services["svc1"]; got.Status != domain.ServiceStopped || got.Checkout.Path != started.Checkouts[0].Path {
		t.Errorf("state = %+v; the service is stopped but its checkout is still recorded", got)
	}
}

func TestStopThenStartKeepsTheWork(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	first := f.mustStart("demo", "")
	sha := commitAt(t, first.Checkouts[0].Path, "work.txt", "x\n")
	f.mustStop("demo", false, false)

	second := f.mustStart("demo", "")
	if !second.Checkouts[0].Created {
		t.Error("the checkout should be created again")
	}
	if head := testkit.RunGit(t, second.Checkouts[0].Path, "rev-parse", "HEAD"); head != sha {
		t.Errorf("restarted at %q, want the imported commit %q: stop and start must not lose work", head, sha)
	}
	if got := f.state("demo").Services["svc1"].Status; got != domain.ServiceProvisioned {
		t.Errorf("status = %s after restarting", got)
	}
}

func TestStopToleratesACheckoutDeletedByHand(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t))
	started := f.mustStart("demo", "")
	os.RemoveAll(started.Checkouts[0].Path)

	res := f.mustStop("demo", false, false)
	if !res.Services[0].Removed {
		t.Errorf("services = %+v", res.Services)
	}
	if got := f.state("demo").Services["svc1"]; got.Status != domain.ServiceStopped || got.Checkout.Path != "" {
		t.Errorf("state = %+v", got)
	}
}

func TestStopNeverDeletesAnUnrecognizableDirectory(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t))
	started := f.mustStart("demo", "")
	path := started.Checkouts[0].Path
	os.RemoveAll(path)
	os.MkdirAll(path, 0o755)
	os.WriteFile(filepath.Join(path, "precious.txt"), []byte("someone else's files"), 0o644)

	for _, force := range []bool{false, true} {
		_, err := f.stop("demo", force, false)
		if d := details(t, err); len(d) != 1 || !strings.Contains(d[0], "svc1") || !strings.Contains(d[0], "not a clone checkout") {
			t.Errorf("force=%v: details = %v", force, d)
		}
		if got, _ := os.ReadFile(filepath.Join(path, "precious.txt")); string(got) != "someone else's files" {
			t.Errorf("force=%v: the files were touched", force)
		}
	}
}

func TestStopErrors(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("fresh", testkit.NewRepo(t)) // created, never started
	f.create("again", testkit.NewRepo(t))
	f.mustStart("again", "")
	f.mustStop("again", false, false)

	tests := []struct {
		name string
		id   string
		code api.ErrorCode
		text string
	}{
		{"a ticket that was never started", "fresh", api.CodeConflict, "nothing to stop"},
		{"an unknown ticket", "nope", api.CodeNotFound, "nope"},
		{"an invalid id", "../etc", api.CodeInvalid, "ticket id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.stop(tt.id, false, false)
			var e *api.Error
			if !errors.As(err, &e) || e.Code != tt.code || !strings.Contains(e.Message, tt.text) {
				t.Errorf("error = %v, want %s mentioning %q", err, tt.code, tt.text)
			}
		})
	}
}
