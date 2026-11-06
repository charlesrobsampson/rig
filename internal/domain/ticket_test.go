package domain_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
)

func TestPolicyGatesDistinguishInheritFromNone(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		wantNil   bool
		wantGates []domain.Gate
	}{
		{"absent means inherit", `{}`, true, nil},
		{"explicit empty means no gates", `{"gates":[]}`, false, []domain.Gate{}},
		{"listed gates", `{"gates":["plan_approval"]}`, false, []domain.Gate{domain.GatePlanApproval}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p domain.Policy
			if err := json.Unmarshal([]byte(tt.json), &p); err != nil {
				t.Fatal(err)
			}
			if (p.Gates == nil) != tt.wantNil {
				t.Fatalf("Gates nil = %v, want %v", p.Gates == nil, tt.wantNil)
			}
			if !tt.wantNil && !reflect.DeepEqual(p.Gates, tt.wantGates) {
				t.Errorf("Gates = %v, want %v", p.Gates, tt.wantGates)
			}
			out, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			var back domain.Policy
			if err := json.Unmarshal(out, &back); err != nil {
				t.Fatal(err)
			}
			if (back.Gates == nil) != tt.wantNil {
				t.Errorf("round trip changed nil-ness of Gates: %s", out)
			}
		})
	}
}

func TestTicketJSONUsesFormatsFieldNames(t *testing.T) {
	const doc = `{
		"id": "ticket-412",
		"title": "Dashboard",
		"source": {"type": "jira", "key": "PROJ-412"},
		"services": [{
			"name": "backend", "repo": "/repos/backend", "branch": "feat/x", "base": "main",
			"path": "./backend", "build": {"dockerfile": "Dockerfile"},
			"start": "go run ./cmd/server", "test": "go test ./...",
			"internal_port": 8080, "healthcheck": "/healthz",
			"policy": {"gates": [], "push": {"mode": "merge"}},
			"auth": "work", "egress": {"allow": ["registry.npmjs.org"]}
		}],
		"dependencies": [{"name": "db", "image": "postgres:16", "env": {"POSTGRES_PASSWORD": "dev"}}],
		"docker": {"network_mode": "internal"},
		"sandbox": "container",
		"env": {"DEBUG": "true"},
		"secrets": ["ANTHROPIC_API_KEY"],
		"harness": "claude-code",
		"auth": "work",
		"egress": {"allow": ["api.example.com"]},
		"policy": {"push": {"mode": "pr"}}
	}`
	var got domain.Ticket
	if err := json.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatal(err)
	}
	svc := got.Services[0]
	checks := []struct {
		name      string
		got, want any
	}{
		{"id", got.ID, "ticket-412"},
		{"source.type", got.Source.Type, "jira"},
		{"source.key", got.Source.Key, "PROJ-412"},
		{"service name", svc.Name, "backend"},
		{"service base", svc.Base, "main"},
		{"service port", svc.InternalPort, 8080},
		{"service dockerfile", svc.Build.Dockerfile, "Dockerfile"},
		{"service push mode", svc.Policy.Push.Mode, domain.PushMerge},
		{"service has explicit empty gates", svc.Policy.Gates != nil && len(svc.Policy.Gates) == 0, true},
		{"service egress", svc.Egress.Allow, []string{"registry.npmjs.org"}},
		{"dependency image", got.Dependencies[0].Image, "postgres:16"},
		{"network mode", got.Docker.NetworkMode, domain.NetworkInternal},
		{"sandbox", got.Sandbox, domain.SandboxContainer},
		{"secrets", got.Secrets, []string{"ANTHROPIC_API_KEY"}},
		{"ticket auth", got.Auth, "work"},
		{"ticket push mode", got.Policy.Push.Mode, domain.PushPR},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
