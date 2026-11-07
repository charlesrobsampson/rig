package git_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/git"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var ctx = context.Background()

// A hostile checkout is one an agent could have rewritten: its config, hooks, and
// attributes may name programs. Each case plants one such program, which touches a
// marker file when it runs, then runs a command that would normally trigger it.
type hostileCase struct {
	name string
	// plant arranges the trap; script is a program that records its execution.
	plant func(t *testing.T, repo *testkit.Repo, script string)
	args  []string
}

func hostileCases() []hostileCase {
	modifyTracked := func(repo *testkit.Repo) {
		repo.WriteFile("a.txt", "changed\n")
	}
	return []hostileCase{
		{
			name: "core.fsmonitor",
			plant: func(t *testing.T, repo *testkit.Repo, script string) {
				repo.Git("config", "core.fsmonitor", script)
			},
			args: []string{"status", "--porcelain"},
		},
		{
			name: "diff.external",
			plant: func(t *testing.T, repo *testkit.Repo, script string) {
				repo.Commit("a.txt", "original\n", "add a")
				repo.Git("config", "diff.external", script)
				modifyTracked(repo)
			},
			args: []string{"diff"},
		},
		{
			name: "a textconv driver chosen through .gitattributes",
			plant: func(t *testing.T, repo *testkit.Repo, script string) {
				repo.WriteFile(".gitattributes", "*.txt diff=evil\n")
				repo.Git("add", ".gitattributes")
				repo.Commit("a.txt", "original\n", "add a")
				repo.Git("config", "diff.evil.textconv", script)
				modifyTracked(repo)
			},
			args: []string{"log", "-p", "-1"},
		},
		{
			name: "a post-checkout hook",
			plant: func(t *testing.T, repo *testkit.Repo, script string) {
				hook := repo.Path(".git/hooks/post-checkout")
				body, _ := os.ReadFile(script)
				if err := os.WriteFile(hook, body, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			args: []string{"checkout", "-q", "-b", "other"},
		},
		{
			name: "a pre-commit hook",
			plant: func(t *testing.T, repo *testkit.Repo, script string) {
				hook := repo.Path(".git/hooks/pre-commit")
				body, _ := os.ReadFile(script)
				if err := os.WriteFile(hook, body, 0o755); err != nil {
					t.Fatal(err)
				}
				repo.WriteFile("b.txt", "new\n")
				repo.Git("add", "b.txt")
			},
			args: []string{"commit", "-q", "-m", "x"},
		},
		{
			name: "a clean filter driver",
			plant: func(t *testing.T, repo *testkit.Repo, script string) {
				repo.WriteFile(".gitattributes", "*.dat filter=evil\n")
				repo.Git("add", ".gitattributes")
				repo.Git("config", "filter.evil.clean", script)
				repo.Commit("a.dat", "original\n", "add a.dat")
				repo.WriteFile("a.dat", "ORIGINAL\n") // same size: git must run the filter to compare
			},
			args: []string{"status", "--porcelain"},
		},
	}
}

// recorder returns a script that records that it ran and otherwise behaves like cat,
// so the planted programs do not break the command they hang off.
func recorder(t *testing.T) (script, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "ran")
	script = filepath.Join(dir, "record.sh")
	body := "#!/bin/sh\ntouch " + marker + "\nfor f in \"$@\"; do [ -f \"$f\" ] && cat \"$f\" && break; done\ncat\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestHardenedRunnerNeutralizesWhatTheRepoControls(t *testing.T) {
	for _, tc := range hostileCases() {
		t.Run(tc.name, func(t *testing.T) {
			script, marker := recorder(t)
			repo := testkit.NewRepo(t)
			tc.plant(t, repo, script)
			os.Remove(marker) // planting may have run the program once

			// The same command, same repo: the trusted runner executes the program,
			// which proves the trap is real; the hardened one must not.
			if _, err := git.Trusted().Run(ctx, repo.Dir, tc.args...); err != nil {
				t.Logf("trusted run returned %v (the program ran anyway if the marker exists)", err)
			}
			if !exists(marker) {
				t.Fatalf("the trap %q did not fire under the trusted runner; this test proves nothing", tc.name)
			}
			resetAfterTrusted(t, repo, tc)
			os.Remove(marker) // the reset itself may have run the program

			if _, err := git.Hardened().Run(ctx, repo.Dir, tc.args...); err != nil {
				t.Logf("hardened run returned %v", err)
			}
			if exists(marker) {
				t.Errorf("the hardened runner executed a program planted through %s", tc.name)
			}
		})
	}
}

// resetAfterTrusted undoes the side effects of the trusted run so the hardened run
// faces the same repository.
func resetAfterTrusted(t *testing.T, repo *testkit.Repo, tc hostileCase) {
	t.Helper()
	switch tc.args[0] {
	case "checkout":
		repo.Git("checkout", "-q", "main")
		repo.Git("branch", "-q", "-D", "other")
	case "commit":
		// the trusted commit may have been rejected by the hook or succeeded; either way
		// restage so the hardened attempt has something to commit.
		repo.WriteFile("c.txt", "more\n")
		repo.Git("add", "c.txt")
	}
}

func TestHardenedRunnerNeverUsesCredentialHelpersOrTheNetwork(t *testing.T) {
	script, marker := recorder(t)
	repo := testkit.NewRepo(t)
	repo.Git("config", "credential.helper", "!"+script)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="rig"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	remote := server.URL + "/repo.git"

	git.Trusted().Run(ctx, repo.Dir, "ls-remote", remote)
	if !exists(marker) {
		t.Fatal("the credential helper did not run under the trusted runner; this test proves nothing")
	}
	os.Remove(marker)

	if _, err := git.Hardened().Run(ctx, repo.Dir, "ls-remote", remote); err == nil {
		t.Error("the hardened runner reached a remote; it must never use the network")
	}
	if exists(marker) {
		t.Error("the hardened runner ran the repository's credential helper")
	}
}

func TestHardenedRunnerIgnoresRepoRedirectionEnvironmentAndUserConfig(t *testing.T) {
	repo := testkit.NewRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	for name, r := range map[string]git.Runner{"trusted": git.Trusted(), "hardened": git.Hardened()} {
		t.Run(name, func(t *testing.T) {
			out, err := r.Run(ctx, repo.Dir, "rev-parse", "--show-toplevel")
			if err != nil || out != repo.Dir {
				t.Errorf("toplevel = %q, %v; ambient GIT_DIR/GIT_WORK_TREE must not redirect the runner", out, err)
			}
		})
	}
}

func TestRunnerErrors(t *testing.T) {
	repo := testkit.NewRepo(t)

	t.Run("a failing command reports git's stderr", func(t *testing.T) {
		_, err := git.Trusted().Run(ctx, repo.Dir, "checkout", "no-such-branch")
		var gitErr *git.Error
		if !errors.As(err, &gitErr) || !strings.Contains(gitErr.Stderr, "no-such-branch") || !strings.Contains(err.Error(), "checkout") {
			t.Fatalf("error = %v, want a *git.Error carrying stderr", err)
		}
	})

	t.Run("outside a repository", func(t *testing.T) {
		_, err := git.Trusted().Run(ctx, t.TempDir(), "status")
		if err == nil || !strings.Contains(err.Error(), "not a git repository") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a cancelled context stops git", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := git.Trusted().Run(cancelled, repo.Dir, "status"); err == nil {
			t.Error("expected an error from a cancelled context")
		}
	})
}

func TestRunnerReturnsTrimmedOutput(t *testing.T) {
	repo := testkit.NewRepo(t)
	out, err := git.Trusted().Run(ctx, repo.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out != "main" {
		t.Errorf("output = %q, %v", out, err)
	}
}
