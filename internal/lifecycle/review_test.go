package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func review(t *testing.T, f *fixture, ticket, service string) (api.ReviewDiffResult, error) {
	t.Helper()
	return f.lc.ReviewDiff(ctx, api.ReviewDiffParams{Ticket: ticket, Service: service})
}

func TestReviewShowsCommittedAndUncommittedWorkOfEachKindOfCheckout(t *testing.T) {
	for _, sandbox := range []domain.Sandbox{domain.SandboxContainer, domain.SandboxHost} {
		t.Run(string(sandbox), func(t *testing.T) {
			f := newFixture(t, sandbox)
			repo := testkit.NewRepo(t)
			f.create("demo", repo)
			res := f.mustStart("demo", sandbox)
			dir := res.Checkouts[0].Path
			os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("c1\nc2\n"), 0o600)
			testkit.RunGit(t, dir, "add", "-A")
			testkit.RunGit(t, dir, "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-qm", "work")
			os.WriteFile(filepath.Join(dir, "later.txt"), []byte("l1\n"), 0o600) // untracked

			got, err := review(t, f, "demo", "")
			if err != nil || len(got.Services) != 1 {
				t.Fatalf("ReviewDiff = %+v, %v", got, err)
			}
			d := got.Services[0]
			if d.Service != "svc1" || d.Branch != "rig/demo/svc1" || d.Base != "main" || d.Error != "" || d.Note != "" {
				t.Errorf("header = %+v", d)
			}
			byPath := map[string]api.FileChange{}
			for _, fc := range d.Files {
				byPath[fc.Path] = fc
			}
			if byPath["committed.txt"].Added != 2 || byPath["later.txt"].Added != 1 || !byPath["later.txt"].Untracked || len(d.Files) != 2 {
				t.Errorf("files = %+v", d.Files)
			}
			if !strings.Contains(d.Patch, "+c2") || !strings.Contains(d.Patch, "+l1") || d.MergeBase == "" {
				t.Errorf("patch = %q, merge base %q", d.Patch, d.MergeBase)
			}
		})
	}
}

func TestAServiceWithNoChangesHasAnEmptyDiff(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxContainer)
	got, err := review(t, f, "demo", "svc1")
	if err != nil || len(got.Services) != 1 || len(got.Services[0].Files) != 0 || got.Services[0].Patch != "" || got.Services[0].Note != "" {
		t.Errorf("ReviewDiff = %+v, %v", got, err)
	}
}

func TestEachServiceGetsItsOwnDiffAndOneCanBeNamed(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	os.WriteFile(filepath.Join(res.Checkouts[0].Path, "only-in-one.txt"), []byte("1\n"), 0o600)
	os.WriteFile(filepath.Join(res.Checkouts[1].Path, "only-in-two.txt"), []byte("2\n"), 0o600)

	all, err := review(t, f, "demo", "")
	if err != nil || len(all.Services) != 2 || all.Services[0].Service != "svc1" || all.Services[1].Service != "svc2" {
		t.Fatalf("ReviewDiff = %+v, %v", all, err)
	}
	if !strings.Contains(all.Services[0].Patch, "only-in-one") || strings.Contains(all.Services[0].Patch, "only-in-two") {
		t.Errorf("svc1's patch is mixed up: %q", all.Services[0].Patch)
	}
	if !strings.Contains(all.Services[1].Patch, "only-in-two") || strings.Contains(all.Services[1].Patch, "only-in-one") {
		t.Errorf("svc2's patch is mixed up: %q", all.Services[1].Patch)
	}
	one, _ := review(t, f, "demo", "svc2")
	if len(one.Services) != 1 || one.Services[0].Service != "svc2" {
		t.Errorf("naming one service = %+v", one)
	}
}

func TestAWorktreeIsComparedWithTheBaseItBranchedFromEvenIfTheBaseMovedOn(t *testing.T) {
	f := newFixture(t, domain.SandboxHost)
	repo := testkit.NewRepo(t)
	f.create("demo", repo)
	res := f.mustStart("demo", domain.SandboxHost)
	repo.Commit("moved-on.txt", "someone else's work\n", "main moves on") // the base advances
	os.WriteFile(filepath.Join(res.Checkouts[0].Path, "mine.txt"), []byte("mine\n"), 0o600)

	got, _ := review(t, f, "demo", "")
	d := got.Services[0]
	if strings.Contains(d.Patch, "moved-on") || !strings.Contains(d.Patch, "+mine") || len(d.Files) != 1 {
		t.Errorf("the diff must show only this ticket's work: files %+v", d.Files)
	}
}

func TestReviewWithoutACheckoutSaysSoAndErrorsAreSpecific(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("fresh", testkit.NewRepo(t))
	got, err := review(t, f, "fresh", "")
	if err != nil || got.Services[0].Note == "" || !strings.Contains(got.Services[0].Note, "not started") {
		t.Errorf("a ticket that was never started: %+v, %v", got, err)
	}

	f.create("stopped", testkit.NewRepo(t))
	f.mustStart("stopped", domain.SandboxContainer)
	f.mustStop("stopped", false, false)
	if got, err := review(t, f, "stopped", ""); err != nil || got.Services[0].Note == "" {
		t.Errorf("a stopped ticket: %+v, %v", got, err)
	}

	for name, tc := range map[string][2]string{"an unknown ticket": {"ghost", ""}, "an unknown service": {"fresh", "nope"}} {
		if _, err := review(t, f, tc[0], tc[1]); apiCode(t, err) != api.CodeNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := review(t, f, "../x", ""); apiCode(t, err) != api.CodeInvalid {
		t.Errorf("a bad ticket id: %v", err)
	}

	f.create("gone", testkit.NewRepo(t))
	res := f.mustStart("gone", domain.SandboxContainer)
	os.RemoveAll(res.Checkouts[0].Path)
	if got, _ := review(t, f, "gone", ""); !strings.Contains(got.Services[0].Note, "missing") {
		t.Errorf("a checkout that vanished: %+v", got.Services[0])
	}
}

func TestAServiceWhoseDiffCannotBeBuiltDoesNotHideTheOthers(t *testing.T) {
	f := newFixture(t, domain.SandboxContainer)
	f.create("demo", testkit.NewRepo(t), testkit.NewRepo(t))
	res := f.mustStart("demo", domain.SandboxContainer)
	os.WriteFile(filepath.Join(res.Checkouts[1].Path, "ok.txt"), []byte("1\n"), 0o600)
	os.RemoveAll(filepath.Join(res.Checkouts[0].Path, ".git")) // the first is no longer a repository
	got, err := review(t, f, "demo", "")
	if err != nil || len(got.Services) != 2 {
		t.Fatalf("ReviewDiff = %+v, %v", got, err)
	}
	if got.Services[0].Error == "" || got.Services[1].Error != "" || !strings.Contains(got.Services[1].Patch, "+1") {
		t.Errorf("services = %+v", got.Services)
	}
}
