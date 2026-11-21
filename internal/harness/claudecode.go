package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/state"
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

// settings is the JSON given to `claude --settings`: the hooks that report state and, if
// costCommand is set, a status line command that receives Claude Code's status JSON (which
// carries the session's cost) and reports it.
func settings(hookCommand, costCommand string) (string, error) {
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
	out := map[string]any{"hooks": events}
	if costCommand != "" {
		out["statusLine"] = map[string]string{"type": "command", "command": costCommand}
	}
	b, err := json.Marshal(out)
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
	r.HookCommand = hookCommandOf(r)
	settingsJSON, err := settings(r.HookCommand, r.CostCommand)
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
	if r.ConfigDir != "" {
		env["CLAUDE_CONFIG_DIR"] = r.ConfigDir
	}
	env["RIG_TICKET"], env["RIG_SERVICE"] = r.Ticket.ID, r.Service

	// The ticket directory holds the files the agent works from and writes to (plan.md,
	// tasks.md, contract.md, manual-tests.md); without this every edit there would ask.
	command := []string{"claude", "--settings", settingsJSON}
	if r.Model != "" {
		command = append(command, "--model", r.Model)
	}
	if addDir := firstNonEmpty(r.AgentTicketDir, r.TicketDir); addDir != "" {
		command = append(command, "--add-dir", addDir)
	}
	// "--" ends the options: --add-dir takes several values, and without it Claude Code
	// reads the prompt as one more directory and starts with no instructions.
	return Launch{Command: append(command, "--", prompt), Env: env, Dir: r.Dir}, nil
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

	writeRules(&b, r)

	b.WriteString("\n## Journal\n")
	for _, rule := range journalRules(hookCommandOf(r)) {
		b.WriteString("- " + rule + "\n")
	}

	artifacts := []string{"plan.md", "tasks.md"}
	if len(r.Ticket.Services) > 1 {
		b.WriteString("\n## contract.md\n")
		for _, rule := range contractRules {
			b.WriteString("- " + rule + "\n")
		}
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

// PhaseNotice is what an agent that is already running is told when its ticket moves
// from one phase to r.Phase: the move, and the rules that apply now. Its prompt was
// built at launch, so without this it would carry on under the old phase's rules.
func PhaseNotice(r Request, from domain.Phase, reason string) string {
	var b strings.Builder
	direction := "to"
	if state.IsBackward(from, r.Phase) {
		direction = "back to"
	}
	fmt.Fprintf(&b, "This ticket moved from %s %s %s.", from, direction, r.Phase)
	if reason = strings.Join(strings.Fields(reason), " "); reason != "" {
		fmt.Fprintf(&b, " Reason: %s.", strings.TrimSuffix(reason, "."))
	}
	fmt.Fprintf(&b, " Current phase: %s. The rules below replace the ones you were given before; carry on under them now.\n\n", r.Phase)
	writeRules(&b, r)
	return b.String()
}

// writeRules writes the rules for r.Phase: the ones every agent gets, the phase's own,
// and how to write manual-tests.md once there is something to test by hand.
func writeRules(b *strings.Builder, r Request) {
	b.WriteString("## Rules\n")
	for _, rule := range commonRules(r) {
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
}

// commonRules name the ticket files the agent may edit, where it sees them: the plan
// phase works in files there, so a rule that allowed only the checkout contradicted it.
func commonRules(r Request) []string {
	files := "plan.md and tasks.md"
	switch multi, later := len(r.Ticket.Services) > 1, !beforeImplement(r.Phase); {
	case multi && later:
		files = "plan.md, tasks.md, manual-tests.md and proposals under \"## Proposed changes\" in contract.md"
	case multi:
		files = "plan.md, tasks.md and proposals under \"## Proposed changes\" in contract.md"
	case later:
		files = "plan.md, tasks.md and manual-tests.md"
	}
	return []string{
		"Edit only your own service's checkout (your working directory) and, in the ticket directory (" + firstNonEmpty(r.AgentTicketDir, r.TicketDir) + "), " + files + ".",
		"Never edit state.json or ticket.yaml.",
		"Request phase transitions; never assume them. Gates are approved by a human only. To request one, leave a journal record (see Journal) of kind \"transition.request\" with data {\"to\": \"<phase>\", \"reason\": \"...\"}, then stop and wait; a human approves and moves the ticket, and you will be told.",
	}
}

// commitRule is said in every phase that changes code, with the reason: an agent's own
// default is to commit only when asked, and one terse line did not beat it.
const commitRule = "Commit each change on your branch as soon as its tests pass, without being asked. rig imports your branch's commits; uncommitted work is not reviewed or pushed."

// contractRules apply only to a ticket with several services, whose contract.md holds
// what they have agreed on (docs/formats.md §11).
var contractRules = []string{
	"Read contract.md (in the ticket directory) before you start: it is what your service and the others have agreed on, and you build against it.",
	"Never edit the \"## Agreed\" section. To change it, append a proposal under \"## Proposed changes\" (the format is in the file's comments) and carry on with what the current contract allows; a human accepts or rejects it, and you will be told.",
	"If a proposal of yours is rejected, work within the agreed contract or propose something else.",
}

var phaseRules = map[domain.Phase][]string{
	domain.PhasePlan:      {"Refine plan.md and tasks.md; do not change your checkout or write implementation code yet.", "Request the implement phase when the plan is ready for approval."},
	domain.PhaseImplement: {"Work through tasks.md in small, meaningful steps.", commitRule, "Run the service's tests before asking to move on."},
	domain.PhaseTest:      {"Run the full test suite and fix failures; keep manual-tests.md accurate.", commitRule, "Request the review phase once tests pass."},
	domain.PhaseReview:    {"Address review feedback only; keep changes small and focused.", commitRule},
	domain.PhasePush:      {"Do not push; pushing is done by rig on the host after approval."},
}

func hookCommandOf(r Request) string {
	if r.HookCommand == "" {
		return "rig agent-event"
	}
	return r.HookCommand
}

// journalRules tell the agent how to leave a record in the ticket's journal through the
// same command its hooks use, so it works in a container and on the host alike
// (docs/formats.md §14).
func journalRules(hookCommand string) []string {
	return []string{
		"To leave a record in the ticket's journal, run `" + hookCommand + ` '{"kind":"feedback","text":"..."}'` + "`" + ` with one line of JSON: "kind" (lowercase, such as "feedback" or "note"), "text", and optionally "data" (a JSON object). rig adds the ticket, service and time itself.`,
		`Use "kind": "feedback" whenever rig or your environment gets in your way: a missing tool, permission or credential, a blocked host, a rule that is unclear, something slow. Say what you were doing, what you expected and what happened, then carry on as best you can.`,
		"Keep the JSON on one line and put no single quote in it (write \\u0027 instead).",
	}
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
