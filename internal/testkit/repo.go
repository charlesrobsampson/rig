package testkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a throwaway git repository on a real filesystem. Tests exercise real git
// (docs: process.md §4), so NewRepo pins the environment to make it deterministic:
// no user or system config, a fixed identity, and fixed commit dates.
type Repo struct {
	t   *testing.T
	Dir string
}

func pinGitEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Rig Test",
		"GIT_AUTHOR_EMAIL":    "test@rig.invalid",
		"GIT_COMMITTER_NAME":  "Rig Test",
		"GIT_COMMITTER_EMAIL": "test@rig.invalid",
		"GIT_AUTHOR_DATE":     "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE":  "2026-01-01T00:00:00Z",
	} {
		t.Setenv(k, v)
	}
}

// NewRepo creates a repository on branch main with one commit.
func NewRepo(t *testing.T) *Repo {
	t.Helper()
	pinGitEnv(t)
	r := &Repo{t: t, Dir: RealTempDir(t)}
	r.Git("init", "-q", "-b", "main")
	r.Commit("README.md", "# test repo\n", "initial commit")
	return r
}

// RealTempDir is t.TempDir with symlinks resolved, because git reports real paths
// (on macOS /var is a link to /private/var) and tests compare against them.
func RealTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Git runs git in the repository and returns trimmed stdout, failing the test on error.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	return RunGit(r.t, r.Dir, args...)
}

func RunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *Repo) WriteFile(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Commit writes a file, commits it, and returns the new commit's id.
func (r *Repo) Commit(rel, content, message string) string {
	r.t.Helper()
	r.WriteFile(rel, content)
	r.Git("add", rel)
	r.Git("commit", "-q", "-m", message)
	return r.Git("rev-parse", "HEAD")
}

func (r *Repo) Path(rel string) string { return filepath.Join(r.Dir, rel) }
