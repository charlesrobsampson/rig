package git_test

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func cloneSpec(repo *testkit.Repo, path, branch, base string) git.Spec {
	return git.Spec{Repo: repo.Dir, Branch: branch, Base: base, Path: path, Kind: domain.CheckoutClone}
}

func hardened(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Hardened().Run(ctx, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// repoWithHistory has enough content that copying its objects would be obvious.
func repoWithHistory(t *testing.T) *testkit.Repo {
	t.Helper()
	repo := testkit.NewRepo(t)
	blob := make([]byte, 1<<20)
	rand.Read(blob)
	for i := 0; i < 3; i++ {
		repo.Commit(fmt.Sprintf("big%d.bin", i), string(blob)+fmt.Sprint(i), fmt.Sprintf("big file %d", i))
	}
	return repo
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func TestProvisionCloneStartsANewBranchAtTheBase(t *testing.T) {
	repo := testkit.NewRepo(t)
	baseCommit := repo.Git("rev-parse", "HEAD")
	repo.Git("checkout", "-q", "-b", "develop")
	repo.Commit("dev.txt", "dev\n", "on develop")
	developCommit := repo.Git("rev-parse", "HEAD")
	repo.Git("checkout", "-q", "main")

	tests := []struct {
		name, base, want string
	}{
		{"a named branch", "develop", developCommit},
		{"the default branch when no base is given", "", baseCommit},
		{"a commit id", baseCommit, baseCommit},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := newPath(t)
			branch := fmt.Sprintf("feat/c%d", i)
			co, err := git.Provision(ctx, cloneSpec(repo, path, branch, tt.base))
			if err != nil {
				t.Fatal(err)
			}
			if co.Kind() != domain.CheckoutClone || co.Path() != path || co.Branch() != branch {
				t.Errorf("checkout = %s %s %s", co.Kind(), co.Path(), co.Branch())
			}
			if got := head(t, path); got != tt.want {
				t.Errorf("HEAD = %s, want %s", got, tt.want)
			}
			if got := hardened(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
				t.Errorf("checked out %q, want %q", got, branch)
			}
			if got := hardened(t, path, "status", "--porcelain"); got != "" {
				t.Errorf("a fresh clone is not clean: %q", got)
			}
		})
	}
}

func TestProvisionCloneContinuesAnExistingBranch(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Git("checkout", "-q", "-b", "feat/existing")
	existing := repo.Commit("f.txt", "earlier work\n", "wip")
	repo.Git("checkout", "-q", "main")

	path := newPath(t)
	if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/existing", "main")); err != nil {
		t.Fatal(err)
	}
	if got := head(t, path); got != existing {
		t.Errorf("HEAD = %s, want the existing branch's tip %s, not the base", got, existing)
	}
}

func TestACloneSharesObjectsInsteadOfCopyingThem(t *testing.T) {
	repo := repoWithHistory(t)
	mainSize := dirSize(t, repo.Path(".git"))
	if mainSize < 3<<20 {
		t.Fatalf("test setup: the main repo is only %d bytes", mainSize)
	}
	path := newPath(t)
	if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/x", "main")); err != nil {
		t.Fatal(err)
	}

	if size := dirSize(t, filepath.Join(path, ".git")); size > 1<<20 {
		t.Errorf("clone .git is %d bytes; it should share the %d-byte object store", size, mainSize)
	}
	alternates, err := os.ReadFile(filepath.Join(path, ".git", "objects", "info", "alternates"))
	if err != nil || !strings.Contains(string(alternates), repo.Path(".git/objects")) {
		t.Errorf("alternates = %q, %v; want the main repo's object store", alternates, err)
	}
	// History reachable only through the shared objects is readable.
	if n := hardened(t, path, "rev-list", "--count", "HEAD"); n != "4" {
		t.Errorf("commits visible in the clone = %s, want 4", n)
	}
	if hardened(t, path, "fsck", "--connectivity-only") != "" {
		t.Error("the clone fails its connectivity check")
	}
}

func TestACommitInTheCloneStaysThereUntilImported(t *testing.T) {
	repo := testkit.NewRepo(t)
	path := newPath(t)
	if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/x", "main")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(path, "agent.txt"), []byte("agent work\n"), 0o644)
	testkit.RunGit(t, path, "add", "agent.txt")
	testkit.RunGit(t, path, "commit", "-q", "-m", "agent commit")
	sha := testkit.RunGit(t, path, "rev-parse", "HEAD")

	if _, err := git.Trusted().Run(ctx, repo.Dir, "cat-file", "-e", sha); err == nil {
		t.Error("the agent's commit appeared in the main repository on its own")
	}
	if branches, _ := git.Branches(ctx, repo.Dir); contains(branches, "feat/x") {
		t.Error("the agent's branch appeared in the main repository on its own")
	}
}

func TestACloneHasNoWritePathIntoTheMainRepository(t *testing.T) {
	repo := testkit.NewRepo(t)
	repo.Git("branch", "other")
	snapshot := func() string {
		var parts []string
		for _, f := range []string{"config", "HEAD"} {
			b, _ := os.ReadFile(repo.Path(".git/" + f))
			parts = append(parts, f, fmt.Sprintf("%x", sha256.Sum256(b)))
		}
		hooks, _ := os.ReadDir(repo.Path(".git/hooks"))
		for _, h := range hooks {
			parts = append(parts, "hook:"+h.Name())
		}
		parts = append(parts, repo.Git("for-each-ref"), repo.Git("worktree", "list"))
		return strings.Join(parts, "\n")
	}

	path := newPath(t)
	if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/x", "main")); err != nil {
		t.Fatal(err)
	}
	before := snapshot()

	// Everything an agent might do inside its clone.
	os.WriteFile(filepath.Join(path, "a.txt"), []byte("x\n"), 0o644)
	for _, args := range [][]string{
		{"add", "a.txt"}, {"commit", "-q", "-m", "work"}, {"branch", "side"}, {"tag", "v1"},
		{"config", "user.name", "someone"}, {"checkout", "-q", "-b", "elsewhere"}, {"gc", "-q"},
		{"update-ref", "refs/heads/main", "HEAD"}, {"remote", "set-url", "origin", "/somewhere/else"},
	} {
		testkit.RunGit(t, path, args...)
	}
	if after := snapshot(); after != before {
		t.Errorf("the main repository changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// readableInContainer checks what git inside the container needs: every object store
// it will look for, starting from the clone's alternates file and following each
// mounted store's own, is mounted at exactly the path named, from that same store.
func readableInContainer(t *testing.T, clonePath string, mounts []git.ObjectsMount) {
	t.Helper()
	at := map[string]string{}
	for _, m := range mounts {
		at[m.Target] = m.Source
	}
	var follow func(objects, file string)
	follow = func(objects, file string) {
		data, err := os.ReadFile(filepath.Join(file, "info", "alternates"))
		if err != nil {
			return
		}
		for _, named := range strings.Fields(string(data)) {
			if !filepath.IsAbs(named) {
				named = filepath.Join(objects, named)
			}
			source, ok := at[named]
			if !ok {
				t.Errorf("git in the container looks for %s, which is not mounted (mounts %+v)", named, mounts)
				continue
			}
			if real, err := filepath.EvalSymlinks(named); err != nil || real != source {
				t.Errorf("%s is mounted from %s, but on the host it is %s (%v)", named, source, real, err)
			}
			follow(named, source)
		}
	}
	objects := filepath.Join(clonePath, ".git", "objects")
	follow(objects, objects)
}

func sources(mounts []git.ObjectsMount) []string {
	var out []string
	for _, m := range mounts {
		if !slices.Contains(out, m.Source) {
			out = append(out, m.Source)
		}
	}
	return out
}

func TestObjectsMountsComeFromTheMainRepositoryAndAreResolved(t *testing.T) {
	t.Run("the main repo's object store, with symlinks resolved", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		link := filepath.Join(testkit.RealTempDir(t), "link-to-repo")
		if err := os.Symlink(repo.Dir, link); err != nil {
			t.Fatal(err)
		}
		spec := cloneSpec(repo, newPath(t), "feat/x", "main")
		spec.Repo = link // reached through a symlink, as a user's path might be
		co, err := git.Provision(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		mounts, err := co.(git.ObjectsMounter).ObjectsMounts()
		if err != nil || strings.Join(sources(mounts), "\n") != repo.Path(".git/objects") {
			t.Errorf("mounts = %+v, %v; want only %s mounted", mounts, err, repo.Path(".git/objects"))
		}
		readableInContainer(t, spec.Path, mounts)
		named, _ := os.ReadFile(filepath.Join(spec.Path, ".git", "objects", "info", "alternates"))
		if strings.TrimSpace(string(named)) != repo.Path(".git/objects") {
			t.Errorf("the new clone's alternates names %q; want the real path %s", named, repo.Path(".git/objects"))
		}
	})

	t.Run("a clone made through a symlink by an older rig", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		link := filepath.Join(testkit.RealTempDir(t), "link-to-repo")
		if err := os.Symlink(repo.Dir, link); err != nil {
			t.Fatal(err)
		}
		path := newPath(t)
		// what earlier rigs did: clone from the path as given, so alternates names the link
		testkit.RunGit(t, repo.Dir, "clone", "-q", "--shared", "--no-checkout", link, path)
		testkit.RunGit(t, path, "checkout", "-q", "-b", "feat/x", "main")
		co, err := git.Open(ctx, git.Spec{Repo: link, Branch: "feat/x", Path: path, Kind: domain.CheckoutClone})
		if err != nil {
			t.Fatal(err)
		}
		mounts, err := co.(git.ObjectsMounter).ObjectsMounts()
		if err != nil || strings.Join(sources(mounts), "\n") != repo.Path(".git/objects") {
			t.Errorf("mounts = %+v, %v; want only %s mounted", mounts, err, repo.Path(".git/objects"))
		}
		readableInContainer(t, path, mounts)
	})

	t.Run("alternates the main repo itself depends on", func(t *testing.T) {
		upstream := testkit.NewRepo(t)
		mainDir := filepath.Join(testkit.RealTempDir(t), "main")
		testkit.RunGit(t, upstream.Dir, "clone", "-q", "--shared", upstream.Dir, mainDir)
		mainRepo := &testkit.Repo{Dir: mainDir}
		spec := git.Spec{Repo: mainDir, Branch: "feat/x", Path: newPath(t), Kind: domain.CheckoutClone}
		co, err := git.Provision(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		mounts, err := co.(git.ObjectsMounter).ObjectsMounts()
		want := []string{mainRepo.Path(".git/objects"), upstream.Path(".git/objects")}
		if err != nil || strings.Join(sources(mounts), "\n") != strings.Join(want, "\n") {
			t.Errorf("mounts = %+v, %v; want %v", mounts, err, want)
		}
		readableInContainer(t, spec.Path, mounts)
	})

	t.Run("a store the main repo borrows through a symlink", func(t *testing.T) {
		upstream := testkit.NewRepo(t)
		link := filepath.Join(testkit.RealTempDir(t), "link-to-upstream")
		if err := os.Symlink(upstream.Dir, link); err != nil {
			t.Fatal(err)
		}
		mainDir := filepath.Join(testkit.RealTempDir(t), "main")
		testkit.RunGit(t, upstream.Dir, "clone", "-q", "--shared", link, mainDir)
		spec := git.Spec{Repo: mainDir, Branch: "feat/x", Path: newPath(t), Kind: domain.CheckoutClone}
		co, err := git.Provision(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		mounts, err := co.(git.ObjectsMounter).ObjectsMounts()
		if err != nil {
			t.Fatal(err)
		}
		readableInContainer(t, spec.Path, mounts)
	})

	t.Run("an agent rewriting its clone's alternates changes nothing", func(t *testing.T) {
		repo := testkit.NewRepo(t)
		path := newPath(t)
		co, err := git.Provision(ctx, cloneSpec(repo, path, "feat/x", "main"))
		if err != nil {
			t.Fatal(err)
		}
		before, err := co.(git.ObjectsMounter).ObjectsMounts()
		if err != nil {
			t.Fatal(err)
		}
		hostile := filepath.Join(path, ".git", "objects", "info", "alternates")
		os.WriteFile(hostile, []byte("/Users/someone/.ssh\n"), 0o644)
		mounts, err := co.(git.ObjectsMounter).ObjectsMounts()
		if err != nil || !slices.Equal(mounts, before) || strings.Contains(fmt.Sprint(mounts), ".ssh") {
			t.Errorf("mounts = %+v, %v; they must come from the main repo, not the clone's alternates file", mounts, err)
		}
	})
}

func TestProvisionCloneCleansUpAfterAFailure(t *testing.T) {
	repo := testkit.NewRepo(t)

	t.Run("an unknown base leaves nothing behind", func(t *testing.T) {
		path := newPath(t)
		if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/x", "no-such-base")); err == nil || !strings.Contains(err.Error(), "no-such-base") {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the checkout directory was left behind: %v", err)
		}
	})

	t.Run("a pre-existing empty directory is left empty", func(t *testing.T) {
		path := newPath(t)
		os.MkdirAll(path, 0o755)
		if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/y", "no-such-base")); err == nil {
			t.Fatal("expected an error")
		}
		if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
			t.Errorf("directory = %v, %v; want it kept but emptied", entries, err)
		}
	})

	t.Run("a non-empty directory is refused untouched", func(t *testing.T) {
		path := newPath(t)
		os.MkdirAll(path, 0o755)
		os.WriteFile(filepath.Join(path, "keep.txt"), []byte("mine"), 0o644)
		if _, err := git.Provision(ctx, cloneSpec(repo, path, "feat/z", "main")); err == nil {
			t.Fatal("expected an error")
		}
		if got, _ := os.ReadFile(filepath.Join(path, "keep.txt")); string(got) != "mine" {
			t.Error("existing files were touched")
		}
	})
}

func TestTwoClonesOfOneRepoAreIndependent(t *testing.T) {
	repo := testkit.NewRepo(t)
	a, b := newPath(t), newPath(t)
	for _, p := range []string{a, b} {
		if _, err := git.Provision(ctx, cloneSpec(repo, p, "feat/same-branch", "main")); err != nil {
			t.Fatalf("two clones may use the same branch name (unlike worktrees): %v", err)
		}
	}
	os.WriteFile(filepath.Join(a, "only-a.txt"), []byte("a\n"), 0o644)
	if _, err := os.Stat(filepath.Join(b, "only-a.txt")); err == nil {
		t.Error("clones share a working tree")
	}
}
