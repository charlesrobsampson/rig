package lifecycle_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// placeFile adds `files: [{path, from}]` to the ticket's only service.
func placeFile(t *testing.T, ticketPath, path, from string) {
	t.Helper()
	file := filepath.Join(ticketPath, "ticket.yaml")
	raw, _ := os.ReadFile(file)
	branch := regexp.MustCompile(`(?m)^(\s+)branch: .*$`)
	m := branch.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no branch line in:\n%s", raw)
	}
	out := branch.ReplaceAll(raw, []byte("$0\n${1}files: [{path: "+path+", from: "+from+"}]"))
	if err := os.WriteFile(file, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hostFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "npmrc")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlacedFilesArriveIgnoredAndAreRefreshedAtEveryStart(t *testing.T) {
	for _, sandbox := range []domain.Sandbox{domain.SandboxHost, domain.SandboxContainer} {
		t.Run(string(sandbox), func(t *testing.T) {
			f, _, _ := agentsFixture(t, withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}))
			path := f.create("demo", testkit.NewRepo(t))
			useAuth(t, path)
			from := hostFile(t, "//registry/:_authToken=${NODE_AUTH_TOKEN}\n")
			placeFile(t, path, ".npmrc", from)

			res := f.mustStart("demo", sandbox)
			checkout := res.Checkouts[0].Path
			if got, _ := os.ReadFile(filepath.Join(checkout, ".npmrc")); !strings.Contains(string(got), "${NODE_AUTH_TOKEN}") {
				t.Fatalf(".npmrc = %q", got)
			}
			if status := testkit.RunGit(t, checkout, "status", "--porcelain"); status != "" {
				t.Errorf("git status = %q; a placed file must be ignored", status)
			}

			os.WriteFile(from, []byte("changed\n"), 0o600)
			f.mustStart("demo", sandbox)
			if got, _ := os.ReadFile(filepath.Join(checkout, ".npmrc")); string(got) != "changed\n" {
				t.Errorf(".npmrc after a second start = %q; it is copied again at every start", got)
			}
		})
	}
}

func TestBadPlacedFilesAreRefusedBeforeAnythingStarts(t *testing.T) {
	cases := []struct {
		name string
		from func(t *testing.T) string
		want string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") }, "nope"},
		{"a directory", func(t *testing.T) string { return t.TempDir() }, "not a regular file"},
		{"a known secret", func(t *testing.T) string { return hostFile(t, "token=secret-WORK_TOKEN\n") }, "WORK_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, dk, term := agentsFixture(t, withAuth(config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}))
			path := f.create("demo", testkit.NewRepo(t))
			useAuth(t, path)
			placeFile(t, path, ".npmrc", c.from(t))

			_, err := f.start("demo", domain.SandboxContainer)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "files[0]") {
				t.Fatalf("err = %v, want it to name files[0] and %q", err, c.want)
			}
			if strings.Contains(err.Error(), "secret-WORK_TOKEN") {
				t.Error("the error shows the secret")
			}
			if _, err := os.Stat(filepath.Join(path, "svc1")); !os.IsNotExist(err) {
				t.Error("a checkout was made")
			}
			if _, err := dk.InspectContainer(ctx, "rig-demo-svc1-agent"); err == nil || len(term.Spawned()) > 0 {
				t.Error("an agent was started")
			}
		})
	}
}
