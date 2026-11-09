package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/harness"
)

func ticketDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ticket(services ...string) domain.Ticket {
	t := domain.Ticket{ID: "demo-1", Title: "Add locale"}
	for _, s := range services {
		t.Services = append(t.Services, domain.Service{Name: s})
	}
	return t
}

func launch(t *testing.T, r harness.Request) harness.Launch {
	t.Helper()
	h, err := harness.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	l, err := h.Command(r)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

var artifacts = map[string]string{"plan.md": "# Plan\nUse a locale header.\n", "tasks.md": "- [ ] add header\n", "contract.md": "# Contract\n## Agreed\nGET /x\n"}

func TestTheLaunchCommandPerPhase(t *testing.T) {
	dir := ticketDir(t, artifacts)
	for _, phase := range []domain.Phase{domain.PhasePlan, domain.PhaseImplement, domain.PhaseTest, domain.PhaseReview, domain.PhasePush} {
		t.Run(string(phase), func(t *testing.T) {
			l := launch(t, harness.Request{Ticket: ticket("api"), Service: "api", Phase: phase, TicketDir: dir, Dir: "/work/api"})
			if l.Command[0] != "claude" || l.Command[1] != "--settings" || len(l.Command) != 4 {
				t.Fatalf("command = %q, want claude --settings <json> <prompt>", l.Command[:2])
			}
			if l.Dir != "/work/api" {
				t.Errorf("dir = %q", l.Dir)
			}
			prompt := l.Command[3]
			if !strings.Contains(prompt, "Current phase: "+string(phase)+".") {
				t.Errorf("prompt does not name the phase %s:\n%s", phase, prompt)
			}
			if !strings.Contains(prompt, "## This phase ("+string(phase)+")") {
				t.Errorf("prompt has no rules for %s", phase)
			}
		})
	}
	t.Run("done has no phase rules but still launches", func(t *testing.T) {
		l := launch(t, harness.Request{Ticket: ticket("api"), Service: "api", Phase: domain.PhaseDone, TicketDir: dir, Dir: "/w"})
		if strings.Contains(l.Command[3], "## This phase") {
			t.Error("a done ticket got phase rules")
		}
	})
}

func TestThePromptCarriesTheArtifactsAndTheRules(t *testing.T) {
	dir := ticketDir(t, artifacts)
	l := launch(t, harness.Request{Ticket: ticket("api"), Service: "api", Phase: domain.PhaseImplement, TicketDir: dir, Dir: "/w"})
	prompt := l.Command[3]
	for _, want := range []string{
		`service "api" of rig ticket demo-1: Add locale`,
		"Use a locale header.", "- [ ] add header",
		"Never edit state.json, ticket.yaml, or the \"## Agreed\" section",
		"## Proposed changes", "Request phase transitions", "manual-tests.md",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestTheContractIsIncludedOnlyForMultiServiceTickets(t *testing.T) {
	dir := ticketDir(t, artifacts)
	single := launch(t, harness.Request{Ticket: ticket("api"), Service: "api", Phase: domain.PhaseImplement, TicketDir: dir, Dir: "/w"})
	if strings.Contains(single.Command[3], "GET /x") {
		t.Error("a single-service prompt included contract.md")
	}
	multi := launch(t, harness.Request{Ticket: ticket("api", "web"), Service: "web", Phase: domain.PhaseImplement, TicketDir: dir, Dir: "/w"})
	if !strings.Contains(multi.Command[3], "GET /x") {
		t.Error("a multi-service prompt left out contract.md")
	}
}

func TestMissingArtifactsAreSkippedNotFatal(t *testing.T) {
	l := launch(t, harness.Request{Ticket: ticket("api", "web"), Service: "api", Phase: domain.PhasePlan, TicketDir: ticketDir(t, map[string]string{"plan.md": "only a plan"}), Dir: "/w"})
	if !strings.Contains(l.Command[3], "only a plan") || strings.Contains(l.Command[3], "## tasks.md") {
		t.Errorf("prompt = %s", l.Command[3])
	}
}

func TestTheHooksReportStateThroughRigAgentEvent(t *testing.T) {
	l := launch(t, harness.Request{Ticket: ticket("api"), Service: "api", Phase: domain.PhasePlan, TicketDir: ticketDir(t, artifacts), Dir: "/w"})
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(l.Command[2]), &s); err != nil {
		t.Fatalf("--settings is not JSON: %v", err)
	}
	want := map[string]string{
		"SessionStart": "ready", "UserPromptSubmit": "working", "PreToolUse": "working",
		"PermissionRequest": "blocked", "Stop": "idle", "SessionEnd": "exited",
	}
	got := map[string]string{}
	for event, groups := range s.Hooks {
		if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Type != "command" {
			t.Fatalf("%s hook shape = %+v", event, groups)
		}
		got[event] = strings.TrimPrefix(groups[0].Hooks[0].Command, "rig agent-event ")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hooks = %v, want %v", got, want)
	}

	custom := launch(t, harness.Request{Ticket: ticket("api"), Service: "api", Phase: domain.PhasePlan, TicketDir: ticketDir(t, artifacts), Dir: "/w", HookCommand: "/opt/rig agent-event"})
	if !strings.Contains(custom.Command[2], `"/opt/rig agent-event idle"`) {
		t.Errorf("a custom hook command was ignored: %s", custom.Command[2])
	}
}

func TestEnvironment(t *testing.T) {
	dir := ticketDir(t, artifacts)
	tk := ticket("api")
	tk.Env = map[string]string{"LOG_LEVEL": "debug", "RIG_TICKET": "evil"}
	tk.Secrets = []string{"DB_PASSWORD"}
	base := harness.Request{Ticket: tk, Service: "api", Phase: domain.PhasePlan, TicketDir: dir, Dir: "/w",
		Secrets: map[string]string{"DB_PASSWORD": "hunter2", "UNRELATED_TOKEN": "leak-me", "ANTHROPIC_API_KEY": "sk-not-listed"}}

	t.Run("bridge network", func(t *testing.T) {
		r := base
		r.Network = domain.NetworkBridge
		want := map[string]string{"RIG_TICKET": "demo-1", "RIG_SERVICE": "api", "LOG_LEVEL": "debug", "DB_PASSWORD": "hunter2"}
		if got := launch(t, r).Env; !reflect.DeepEqual(got, want) {
			t.Errorf("env = %v, want %v (a ticket cannot override RIG_TICKET; unlisted secrets stay out)", got, want)
		}
	})
	t.Run("internal network disables update checks and telemetry", func(t *testing.T) {
		r := base
		r.Network = domain.NetworkInternal
		env := launch(t, r).Env
		for _, k := range []string{"DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY", "DISABLE_ERROR_REPORTING"} {
			if env[k] != "1" {
				t.Errorf("%s = %q, want 1", k, env[k])
			}
		}
		if _, ok := env["UNRELATED_TOKEN"]; ok {
			t.Error("an unlisted secret reached the environment")
		}
	})
	t.Run("a listed secret with no value is an error naming it, not its value", func(t *testing.T) {
		r := base
		r.Secrets = nil
		h, _ := harness.Get("")
		_, err := h.Command(r)
		if err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") {
			t.Errorf("error = %v", err)
		}
	})
}

func TestSecretValuesNeverAppearInThePromptOrCommand(t *testing.T) {
	dir := ticketDir(t, artifacts)
	tk := ticket("api")
	tk.Secrets = []string{"DB_PASSWORD"}
	l := launch(t, harness.Request{Ticket: tk, Service: "api", Phase: domain.PhaseImplement, TicketDir: dir, Dir: "/w",
		Secrets: map[string]string{"DB_PASSWORD": "hunter2-sentinel", "KEY": "sk-sentinel"},
		Profile: &config.AuthProfile{Type: domain.AuthAPIKey, Secrets: []string{"KEY"}}})
	for _, arg := range l.Command {
		if strings.Contains(arg, "hunter2-sentinel") || strings.Contains(arg, "sk-sentinel") {
			t.Errorf("a secret value appears in the command line: %.80s", arg)
		}
	}
}

func TestAuthProfilesMapToTheVariablesClaudeCodeReads(t *testing.T) {
	secrets := map[string]string{"TOKEN": "t0k", "AWS_ACCESS_KEY_ID": "akid", "AWS_SECRET_ACCESS_KEY": "shh", "OTHER": "no"}
	tests := []struct {
		name    string
		profile config.AuthProfile
		want    map[string]string
		wantErr string
	}{
		{"host login passes nothing", config.AuthProfile{Type: domain.AuthHostLogin}, map[string]string{}, ""},
		{"oauth token", config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"TOKEN"}}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t0k"}, ""},
		{"api key", config.AuthProfile{Type: domain.AuthAPIKey, Secrets: []string{"TOKEN"}}, map[string]string{"ANTHROPIC_API_KEY": "t0k"}, ""},
		{"auth token with a gateway url", config.AuthProfile{Type: domain.AuthAuthToken, Secrets: []string{"TOKEN"}, Env: map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example"}},
			map[string]string{"ANTHROPIC_AUTH_TOKEN": "t0k", "ANTHROPIC_BASE_URL": "https://gw.example"}, ""},
		{"bedrock keeps each secret's own name", config.AuthProfile{Type: domain.AuthBedrock, Secrets: []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}, Env: map[string]string{"AWS_REGION": "us-east-1"}},
			map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_ACCESS_KEY_ID": "akid", "AWS_SECRET_ACCESS_KEY": "shh", "AWS_REGION": "us-east-1"}, ""},
		{"vertex", config.AuthProfile{Type: domain.AuthVertex, Env: map[string]string{"CLOUD_ML_REGION": "us-east5"}},
			map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "CLOUD_ML_REGION": "us-east5"}, ""},
		{"foundry", config.AuthProfile{Type: domain.AuthFoundry}, map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1"}, ""},
		{"a missing secret value", config.AuthProfile{Type: domain.AuthAPIKey, Secrets: []string{"MISSING"}}, nil, `"MISSING"`},
		{"two secrets for a single-token type", config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"TOKEN", "OTHER"}}, nil, "exactly one secret"},
		{"no secret for a single-token type", config.AuthProfile{Type: domain.AuthOAuthToken}, nil, "exactly one secret"},
		{"an unknown type", config.AuthProfile{Type: "magic"}, nil, "unknown auth type"},
	}
	h, _ := harness.Get("claude-code")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := h.AuthEnv(tt.profile, secrets)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AuthEnv = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestAuthEnvReachesTheLaunchEnvironment(t *testing.T) {
	l := launch(t, harness.Request{Ticket: ticket("api"), Service: "api", Phase: domain.PhasePlan, TicketDir: ticketDir(t, artifacts), Dir: "/w",
		Profile: &config.AuthProfile{Type: domain.AuthOAuthToken, Secrets: []string{"TOKEN"}}, Secrets: map[string]string{"TOKEN": "t0k"}})
	if l.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "t0k" {
		t.Errorf("env = %v", l.Env)
	}
}

func TestUnknownHarnessesAndBadRequests(t *testing.T) {
	if _, err := harness.Get("cursor"); err == nil || !strings.Contains(err.Error(), "cursor") || !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("Get(cursor) = %v, want an error naming it and the supported harness", err)
	}
	for _, name := range []string{"", "claude-code"} {
		if h, err := harness.Get(name); err != nil || h.Name() != "claude-code" {
			t.Errorf("Get(%q) = %v, %v", name, h, err)
		}
	}
	h, _ := harness.Get("")
	for name, r := range map[string]harness.Request{
		"no service":      {Ticket: ticket("api"), Dir: "/w"},
		"no directory":    {Ticket: ticket("api"), Service: "api"},
		"unknown service": {Ticket: ticket("api"), Service: "web", Dir: "/w"},
	} {
		if _, err := h.Command(r); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
