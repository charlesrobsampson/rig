package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
)

const manifestExample = `
id: "ticket-412"
title: "Full-stack feature for user dashboard"
source: { type: "jira", key: "PROJ-412" }

services:
  - name: backend
    repo: /path/to/backendRepo
    branch: feat/auth-backend-412
    path: ./backend
    build: { dockerfile: Dockerfile }
    start: "go run ./cmd/server"
    test: "go test ./..."
    internal_port: 8080
    healthcheck: "/healthz"
    policy:
      gates: [plan_approval, review_approval]
      push: { mode: pr }
  - name: frontend
    repo: /path/to/frontendRepo
    branch: feat/auth-frontend-412
    path: ./frontend
    start: "bun dev"
    test: "bun test"
    internal_port: 3000
    policy:
      gates: []
      push: { mode: merge }

dependencies:
  - name: db
    image: postgres:16
    env: { POSTGRES_PASSWORD: dev }

docker: { network_mode: bridge }
sandbox: container
env: { DEBUG: "true" }
secrets: [ANTHROPIC_API_KEY]
harness: claude-code
`

const minimal = `
id: demo
title: Demo
services:
  - name: api
    repo: /repos/api
    branch: feat/x
`

func parse(t *testing.T, doc string) (domain.Ticket, error) {
	t.Helper()
	return config.ParseTicket([]byte(doc), config.TicketOptions{RepoCheck: func(string) error { return nil }})
}

func TestParseTicketAcceptsTheManifestExample(t *testing.T) {
	got, err := parse(t, manifestExample)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ticket-412" || got.Source.Type != "jira" || got.Source.Key != "PROJ-412" {
		t.Errorf("header = %+v", got)
	}
	if len(got.Services) != 2 || len(got.Dependencies) != 1 {
		t.Fatalf("services=%d dependencies=%d", len(got.Services), len(got.Dependencies))
	}
	backend, frontend := got.Services[0], got.Services[1]
	if backend.Build.Dockerfile != "Dockerfile" || backend.InternalPort != 8080 || backend.Policy.Push.Mode != domain.PushPR {
		t.Errorf("backend = %+v", backend)
	}
	if len(backend.Policy.Gates) != 2 {
		t.Errorf("backend gates = %v", backend.Policy.Gates)
	}
	if frontend.Policy.Gates == nil || len(frontend.Policy.Gates) != 0 {
		t.Errorf("frontend gates = %#v, want an explicit empty list (no gates, not inherit)", frontend.Policy.Gates)
	}
	if got.Docker.NetworkMode != domain.NetworkBridge || got.Sandbox != domain.SandboxContainer {
		t.Errorf("docker/sandbox = %v / %v", got.Docker.NetworkMode, got.Sandbox)
	}
}

func TestParseTicketAppliesDefaults(t *testing.T) {
	got, err := parse(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if got.Services[0].Path != "./api" {
		t.Errorf("path = %q, want ./api", got.Services[0].Path)
	}
	if got.Source.Type != "manual" {
		t.Errorf("source type = %q, want manual", got.Source.Type)
	}
	if got.Services[0].Policy.Gates != nil {
		t.Errorf("absent gates must stay nil (inherit), got %#v", got.Services[0].Policy.Gates)
	}
}

func TestParseTicketRejectsInvalidTickets(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string // every substring must appear in the error
	}{
		{"missing id", "title: T\nservices:\n  - {name: api, repo: /r, branch: b}\n", []string{"id", "required"}},
		{"missing title", "id: demo\nservices:\n  - {name: api, repo: /r, branch: b}\n", []string{"title", "required"}},
		{"no services", "id: demo\ntitle: T\n", []string{"services", "at least one"}},
		{"bad ticket id", "id: Demo_1\ntitle: T\nservices:\n  - {name: api, repo: /r, branch: b}\n", []string{"ticket id", "Demo_1"}},
		{"service missing repo", "id: d\ntitle: T\nservices:\n  - {name: api, branch: b}\n", []string{"services[0].repo", "required"}},
		{"service missing branch", "id: d\ntitle: T\nservices:\n  - {name: api, repo: /r}\n", []string{"services[0].branch", "required"}},
		{"service missing name", "id: d\ntitle: T\nservices:\n  - {repo: /r, branch: b}\n", []string{"services[0].name"}},
		{"bad service name", "id: d\ntitle: T\nservices:\n  - {name: API, repo: /r, branch: b}\n", []string{"services[0].name", "API"}},
		{"relative repo", "id: d\ntitle: T\nservices:\n  - {name: api, repo: ../r, branch: b}\n", []string{"services[0].repo", "absolute"}},
		{"duplicate service names", "id: d\ntitle: T\nservices:\n  - {name: api, repo: /r, branch: b}\n  - {name: api, repo: /s, branch: c, path: ./other}\n", []string{"duplicate name", "api"}},
		{"service and dependency share a name", minimal + "dependencies:\n  - {name: api, image: x}\n", []string{"duplicate name", "api"}},
		{"dependency without image", minimal + "dependencies:\n  - {name: db}\n", []string{"dependencies[0].image", "required"}},
		{"same path twice", "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b, path: ./x}\n  - {name: b, repo: /s, branch: c, path: ./x}\n", []string{"path", "./x"}},
		{"nested paths", "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b, path: ./x}\n  - {name: b, repo: /s, branch: c, path: ./x/sub}\n", []string{"path", "inside"}},
		{"path escapes the ticket dir", "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b, path: ../out}\n", []string{"services[0].path", "inside the ticket"}},
		{"absolute path", "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b, path: /abs}\n", []string{"services[0].path", "relative"}},
		{"same repo and branch twice", "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b}\n  - {name: b, repo: /r, branch: b}\n", []string{"branch", "/r"}},
		{"port out of range", "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b, internal_port: 70000}\n", []string{"internal_port", "70000"}},
		{"secret with inline value", minimal + "secrets: [\"TOKEN=abc\"]\n", []string{"secrets", "names only"}},
		{"secret with bad name", minimal + "secrets: [my-token]\n", []string{"secrets", "my-token"}},
		{"duplicate secrets", minimal + "secrets: [A_KEY, A_KEY]\n", []string{"secrets", "duplicate", "A_KEY"}},
		{"anthropic key in env", minimal + "env: { KEY: sk-ant-api03-abcdefghijklmnopqrstuvwxyz }\n", []string{"env.KEY", "secrets"}},
		{"github token in env", minimal + "env: { GH: ghp_abcdefghijklmnopqrstuvwxyz0123456789 }\n", []string{"env.GH"}},
		{"aws key in env", minimal + "env: { AWS: AKIAIOSFODNN7EXAMPLE }\n", []string{"env.AWS"}},
		{"private key in env", minimal + "env: { PEM: \"-----BEGIN RSA PRIVATE KEY-----\" }\n", []string{"env.PEM"}},
		{"high entropy value in env", minimal + "env: { BLOB: aZ3kLm9QpR7sT2vXyB5nC8dE1fG4hJ6uWq0 }\n", []string{"env.BLOB"}},
		{"secret in dependency env", minimal + "dependencies:\n  - name: db\n    image: x\n    env: { TOKEN: sk-ant-api03-abcdefghijklmnopqrstuvwxyz }\n", []string{"dependencies[0].env.TOKEN"}},
		{"unknown top-level key", minimal + "bogus: 1\n", []string{"bogus", "line"}},
		{"unknown service key", "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b, servics: x}\n", []string{"servics", "line"}},
		{"invalid enum", minimal + "sandbox: vm\n", []string{"vm"}},
		{"invalid gate", "id: d\ntitle: T\nservices:\n  - name: a\n    repo: /r\n    branch: b\n    policy: { gates: [nope] }\n", []string{"nope"}},
		{"empty file", "", []string{"empty"}},
		{"not a mapping", "- a\n- b\n", []string{"ticket.yaml"}},
		{"several problems reported together", "id: Bad_ID\nservices:\n  - {name: api, branch: b}\n", []string{"ticket id", "title", "services[0].repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.doc)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestParseTicketAcceptsHarmlessValues(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{"short words and flags", "{ DEBUG: \"true\", API_TIMEOUT: 30s, DB_HOST: mock-db-container }"},
		{"a dev password", "{ POSTGRES_PASSWORD: dev }"},
		{"urls", "{ OLLAMA: \"http://localhost:11434\", HEALTH: \"http://api:8080/healthz\" }"},
		{"a git sha", "{ COMMIT: 0123456789abcdef0123456789abcdef01234567 }"},
		{"paths and sentences", "{ DATA: /var/lib/app/data, GREETING: \"hello there, how are you today\" }"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parse(t, minimal+"env: "+tt.env+"\n"); err != nil {
				t.Errorf("rejected harmless env: %v", err)
			}
		})
	}
}

func TestParseTicketAllowsTheSameContainerPortInDifferentServices(t *testing.T) {
	doc := "id: d\ntitle: T\nservices:\n  - {name: a, repo: /r, branch: b, internal_port: 8080}\n  - {name: b, repo: /s, branch: c, internal_port: 8080}\n"
	if _, err := parse(t, doc); err != nil {
		t.Errorf("container ports live in separate containers and must not conflict: %v", err)
	}
}

func TestParseTicketChecksRepositoriesThroughTheInjectedCheck(t *testing.T) {
	doc := "id: d\ntitle: T\nservices:\n  - {name: a, repo: /missing/repo, branch: b}\n"
	_, err := config.ParseTicket([]byte(doc), config.TicketOptions{RepoCheck: func(p string) error {
		return errors.New("no such repository " + p)
	}})
	if err == nil || !strings.Contains(err.Error(), "services[0].repo") || !strings.Contains(err.Error(), "/missing/repo") {
		t.Fatalf("error = %v, want one naming services[0].repo and the path", err)
	}
}

func TestParseTicketDefaultRepoCheckRequiresAnExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		repo    string
		wantErr bool
	}{
		{"existing directory", dir, false},
		{"missing path", filepath.Join(dir, "nope"), true},
		{"a file, not a directory", file, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := "id: d\ntitle: T\nservices:\n  - {name: a, repo: " + tt.repo + ", branch: b}\n"
			_, err := config.ParseTicket([]byte(doc), config.TicketOptions{})
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadTicketReadsAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ticket.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadTicket(path, config.TicketOptions{RepoCheck: func(string) error { return nil }})
	if err != nil || got.ID != "demo" {
		t.Fatalf("LoadTicket = %+v, %v", got, err)
	}
	if _, err := config.LoadTicket(filepath.Join(dir, "missing.yaml"), config.TicketOptions{}); err == nil {
		t.Error("missing file did not error")
	}
}
