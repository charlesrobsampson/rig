package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// ClaudeCode launches Claude Code interactively with hooks that report its state.
type ClaudeCode struct{}

func (ClaudeCode) Name() string { return "claude-code" }

// hooks are the events that tell rig what the agent is doing (docs/spikes/03-tmux.md
// §2). PermissionRequest, not Notification, marks blocked: it fires at once.
var hooks = []struct{ event, matcher, state string }{
	{"SessionStart", "", "ready"},
	{"UserPromptSubmit", "", "working"},
	{"PreToolUse", "*", "working"},
	{"PermissionRequest", "*", "blocked"},
	{"Stop", "", "idle"},
	{"SessionEnd", "", "exited"},
}

func settings(hookCommand string) (string, error) {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type group struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}
	events := map[string][]group{}
	for _, h := range hooks {
		events[h.event] = []group{{Matcher: h.matcher, Hooks: []hook{{Type: "command", Command: hookCommand + " " + h.state}}}}
	}
	b, err := json.Marshal(map[string]any{"hooks": events})
	return string(b), err
}

func (c ClaudeCode) Command(r Request) (Launch, error) {
	if r.Service == "" || r.Dir == "" {
		return Launch{}, fmt.Errorf("a launch needs a service and a directory")
	}
	var svc *domain.Service
	for i := range r.Ticket.Services {
		if r.Ticket.Services[i].Name == r.Service {
			svc = &r.Ticket.Services[i]
		}
	}
	if svc == nil {
		return Launch{}, fmt.Errorf("ticket %s has no service %q", r.Ticket.ID, r.Service)
	}
	hookCommand := r.HookCommand
	if hookCommand == "" {
		hookCommand = "rig agent-event"
	}
	settingsJSON, err := settings(hookCommand)
	if err != nil {
		return Launch{}, err
	}
	prompt, err := Prompt(r)
	if err != nil {
		return Launch{}, err
	}

	env := map[string]string{"RIG_TICKET": r.Ticket.ID, "RIG_SERVICE": r.Service}
	// Without a proxy the agent has no network anyway; these stop it trying to reach
	// hosts the egress allowlist denies (docs/spikes/05-egress.md §4).
	if r.Network == domain.NetworkInternal {
		env["DISABLE_AUTOUPDATER"] = "1"
		env["DISABLE_TELEMETRY"] = "1"
		env["DISABLE_ERROR_REPORTING"] = "1"
	}
	for k, v := range r.Ticket.Env {
		env[k] = v
	}
	for _, name := range r.Ticket.Secrets {
		v, ok := r.Secrets[name]
		if !ok {
			return Launch{}, fmt.Errorf("secret %q (listed in the ticket's secrets) has no value", name)
		}
		env[name] = v
	}
	if r.Profile != nil {
		auth, err := c.AuthEnv(*r.Profile, r.Secrets)
		if err != nil {
			return Launch{}, err
		}
		for k, v := range auth {
			env[k] = v
		}
	}
	// Rig's variables come last: nothing a ticket sets may re-point the agent's reports.
	env["RIG_TICKET"], env["RIG_SERVICE"] = r.Ticket.ID, r.Service

	// The ticket directory holds the files the agent works from and writes to (plan.md,
	// tasks.md, contract.md, manual-tests.md); without this every edit there would ask.
	command := []string{"claude", "--settings", settingsJSON}
	if addDir := firstNonEmpty(r.AgentTicketDir, r.TicketDir); addDir != "" {
		command = append(command, "--add-dir", addDir)
	}
	return Launch{Command: append(command, prompt), Env: env, Dir: r.Dir}, nil
}

// AuthEnv maps a profile to what Claude Code reads (docs/spikes/04-agent-container.md §7).
// A profile's secrets are passed under the variable that fits its type; for the cloud
// types each listed secret keeps its own name (AWS_ACCESS_KEY_ID and so on).
func (ClaudeCode) AuthEnv(p config.AuthProfile, secrets map[string]string) (map[string]string, error) {
	env := map[string]string{}
	value := func(name string) (string, error) {
		v, ok := secrets[name]
		if !ok {
			return "", fmt.Errorf("auth secret %q has no value", name)
		}
		return v, nil
	}
	single := func(variable string) error {
		if len(p.Secrets) != 1 {
			return fmt.Errorf("%s profiles take exactly one secret, got %d", p.Type, len(p.Secrets))
		}
		v, err := value(p.Secrets[0])
		env[variable] = v
		return err
	}

	var err error
	switch p.Type {
	case domain.AuthHostLogin:
		// the machine's own login is used; nothing to pass
	case domain.AuthOAuthToken:
		err = single("CLAUDE_CODE_OAUTH_TOKEN")
	case domain.AuthAPIKey:
		err = single("ANTHROPIC_API_KEY")
	case domain.AuthAuthToken:
		err = single("ANTHROPIC_AUTH_TOKEN")
	case domain.AuthBedrock, domain.AuthVertex, domain.AuthFoundry:
		env["CLAUDE_CODE_USE_"+strings.ToUpper(string(p.Type))] = "1"
		for _, name := range p.Secrets {
			v, verr := value(name)
			if verr != nil {
				return nil, verr
			}
			env[name] = v
		}
	default:
		return nil, fmt.Errorf("unknown auth type %q", p.Type)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range p.Env {
		env[k] = v
	}
	return env, nil
}

// Prompt builds the text that seeds the agent: who and what, the phase's rules, and the
// ticket's artifacts. It never contains secret values.
func Prompt(r Request) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the agent for service %q of rig ticket %s: %s.\n", r.Service, r.Ticket.ID, r.Ticket.Title)
	fmt.Fprintf(&b, "Current phase: %s.\n\n", r.Phase)

	b.WriteString("## Rules\n")
	for _, rule := range commonRules {
		b.WriteString("- " + rule + "\n")
	}
	if rules, ok := phaseRules[r.Phase]; ok {
		b.WriteString("\n## This phase (" + string(r.Phase) + ")\n")
		for _, rule := range rules {
			b.WriteString("- " + rule + "\n")
		}
	}
	if !beforeImplement(r.Phase) {
		b.WriteString("\n## manual-tests.md\n")
		for _, rule := range manualTestsRules {
			b.WriteString("- " + rule + "\n")
		}
	}

	artifacts := []string{"plan.md", "tasks.md"}
	if len(r.Ticket.Services) > 1 {
		artifacts = append(artifacts, "contract.md")
	}
	for _, name := range artifacts {
		content, err := os.ReadFile(filepath.Join(r.TicketDir, name))
		switch {
		case os.IsNotExist(err):
			continue
		case err != nil:
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", name, indent(string(content)))
	}
	return b.String(), nil
}

var commonRules = []string{
	"Edit only files inside your own service's checkout (your working directory).",
	"Never edit state.json, ticket.yaml, or the \"## Agreed\" section of contract.md.",
	"To change an agreed contract, append a proposal under \"## Proposed changes\" in contract.md; a human accepts it.",
	"Request phase transitions; never assume them. Gates are approved by a human only.",
}

var phaseRules = map[domain.Phase][]string{
	domain.PhasePlan:      {"Refine plan.md and tasks.md; do not write implementation code yet.", "Request the implement phase when the plan is ready for approval."},
	domain.PhaseImplement: {"Work through tasks.md, committing small, meaningful changes on your branch.", "Run the service's tests before asking to move on."},
	domain.PhaseTest:      {"Run the full test suite and fix failures; keep manual-tests.md accurate.", "Request the review phase once tests pass."},
	domain.PhaseReview:    {"Address review feedback only; keep changes small and focused."},
	domain.PhasePush:      {"Do not push; pushing is done by rig on the host after approval."},
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// beforeImplement is true while there is nothing to test by hand yet.
func beforeImplement(p domain.Phase) bool { return p == domain.PhasePlan || p == "" }

// manualTestsRules tell the agent how to write the file a human follows to check the
// result by hand. Addresses are placeholders because only the running ticket knows its
// ports (docs/formats.md §12).
var manualTestsRules = []string{
	"Keep manual-tests.md (in the ticket directory) up to date as you build: steps a human can follow to check the result by hand.",
	"Each step is a heading `### <id>: <title>` (the id uses lowercase letters, digits and dashes and is unique), followed by what to do and, where it helps, shell commands in code blocks.",
	"Never write host addresses or ports. Use placeholders: {{<service>.url}}, {{<service>.port}} and {{<service>.internal_url}} for your services, {{<dependency>.host}} for a database or other sidecar, {{ticket.id}}; rig fills them in from the running ticket.",
	"Use only those placeholders: an unknown one makes the whole file unreadable for the human.",
}
