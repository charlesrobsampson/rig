package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAPlacedFileIsWrittenAndIgnored(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k.kind), func(t *testing.T) {
			repo := testkit.NewRepo(t)
			co := provision(t, repo, k.kind, "feat")
			for _, content := range []string{"first", "second"} {
				if err := co.Place(ctx, ".npmrc", []byte(content)); err != nil {
					t.Fatal(err)
				}
				if got := read(t, filepath.Join(co.Path(), ".npmrc")); got != content {
					t.Errorf(".npmrc = %q, want %q: a placed file is replaced at every start", got, content)
				}
			}
			if err := co.Place(ctx, "config/local.json", []byte("{}")); err != nil {
				t.Fatal(err)
			}
			if d, err := co.IsDirty(ctx); err != nil || d.Untracked+d.Modified > 0 {
				t.Errorf("dirty = %+v, %v; placed files must be ignored by git", d, err)
			}
		})
	}
}

func TestAPlacedFileAlreadyIgnoredLeavesTheExcludesAlone(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Commit(".gitignore", ".npmrc\n", "ignore npmrc")
	exclude := filepath.Join(repo.Dir, ".git", "info", "exclude")
	before, _ := os.ReadFile(exclude)
	co := provision(t, repo, domain.CheckoutWorktree, "feat")
	if err := co.Place(ctx, ".npmrc", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(exclude); string(after) != string(before) {
		t.Errorf("the main repository's excludes changed though .gitignore already covers the file:\n%s", after)
	}
}

func TestACloneIgnoresWhatTheMainRepositoryExcludes(t *testing.T) {
	repo := testkit.NewRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Dir, ".git", "info", "exclude"), []byte("# local\n.env.local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	co := provision(t, repo, domain.CheckoutClone, "feat")
	os.WriteFile(filepath.Join(co.Path(), ".env.local"), []byte("X=1"), 0o644)
	if d, err := co.IsDirty(ctx); err != nil || d.Untracked > 0 {
		t.Errorf("dirty = %+v, %v; a clone must ignore what the main repository ignores locally", d, err)
	}
}

// A container agent can plant links in its clone that point anywhere on the host.
func TestPlacingNeverWritesThroughALink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "victim")
	setup := map[string]func(t *testing.T, path string){
		"the file is a link": func(t *testing.T, path string) {
			os.Symlink(outside, filepath.Join(path, ".npmrc"))
		},
		"a parent directory is a link": func(t *testing.T, path string) {
			os.Symlink(filepath.Dir(outside), filepath.Join(path, "config"))
		},
		"the excludes file is a link": func(t *testing.T, path string) {
			exclude := filepath.Join(path, ".git", "info", "exclude")
			os.Remove(exclude)
			os.Symlink(outside, exclude)
		},
	}
	for name, plant := range setup {
		t.Run(name, func(t *testing.T) {
			os.WriteFile(outside, []byte("untouched"), 0o644)
			repo := testkit.NewRepo(t)
			co := provision(t, repo, domain.CheckoutClone, "feat")
			plant(t, co.Path())
			target := ".npmrc"
			if strings.Contains(name, "parent") {
				target = "config/victim"
			}
			err := co.Place(ctx, target, []byte("planted"))
			if got := read(t, outside); got != "untouched" {
				t.Fatalf("rig wrote through a link: %q (err %v)", got, err)
			}
			if name == "the file is a link" {
				if err != nil || read(t, filepath.Join(co.Path(), ".npmrc")) != "planted" {
					t.Errorf("a link where the file goes is replaced by the file: %v", err)
				}
			} else if err == nil {
				t.Error("placing through a linked directory or excludes file must be refused")
			}
		})
	}
}
