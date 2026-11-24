// Package domain holds the types shared by config, state, policy, and the API:
// tickets, services, policies, and the enums whose string forms are fixed by
// docs/formats.md.
package domain

import (
	"fmt"
	"regexp"
)

type Ticket struct {
	ID           string            `json:"id" yaml:"id"`
	Title        string            `json:"title" yaml:"title"`
	Source       Source            `json:"source" yaml:"source"`
	Services     []Service         `json:"services" yaml:"services"`
	Dependencies []Dependency      `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Docker       Docker            `json:"docker" yaml:"docker,omitempty"`
	Sandbox      Sandbox           `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
	Env          map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Secrets      []string          `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Harness      string            `json:"harness,omitempty" yaml:"harness,omitempty"`
	Auth         string            `json:"auth,omitempty" yaml:"auth,omitempty"`
	Egress       Egress            `json:"egress" yaml:"egress,omitempty"`
	// AgentConfig lists directories of agent configuration (subagents, skills, hooks)
	// every agent of the ticket gets. Nil inherits the global default; empty means none.
	AgentConfig []string `json:"agent_config,omitempty" yaml:"agent_config,omitempty"`
	Policy      Policy   `json:"policy" yaml:"policy,omitempty"`
	// Review puts a reviewer agent on one service: rig runs rounds of review between
	// it and that service's own agent (docs/formats.md §4).
	Review *Review `json:"review,omitempty" yaml:"review,omitempty"`
}

// ReviewerService is the name the reviewer's agent goes by in state, events and
// commands; no service of a ticket with a review may use it.
const ReviewerService = "review"

type Review struct {
	// Of is the service under review; empty means the ticket's only service.
	Of          string   `json:"of,omitempty" yaml:"of,omitempty"`
	AgentConfig []string `json:"agent_config,omitempty" yaml:"agent_config,omitempty"`
	Model       string   `json:"model,omitempty" yaml:"model,omitempty"`
	// Copy lists paths the repository ignores (installed dependencies) copied from the
	// coder's checkout into each round's copy, so the reviewer can run the code at once.
	Copy      []string `json:"copy,omitempty" yaml:"copy,omitempty"`
	MaxRounds int      `json:"max_rounds,omitempty" yaml:"max_rounds,omitempty"`
	// BudgetUSD caps what the coder and reviewer may spend in one review loop; 0 is no cap.
	BudgetUSD float64 `json:"budget_usd,omitempty" yaml:"budget_usd,omitempty"`
}

type Source struct {
	Type string `json:"type,omitempty" yaml:"type,omitempty"`
	Key  string `json:"key,omitempty" yaml:"key,omitempty"`
}

type Service struct {
	Name         string  `json:"name" yaml:"name"`
	Repo         string  `json:"repo" yaml:"repo"`
	Branch       string  `json:"branch" yaml:"branch"`
	Base         string  `json:"base,omitempty" yaml:"base,omitempty"`
	Path         string  `json:"path,omitempty" yaml:"path,omitempty"`
	Build        Build   `json:"build" yaml:"build,omitempty"`
	Start        string  `json:"start,omitempty" yaml:"start,omitempty"`
	Test         string  `json:"test,omitempty" yaml:"test,omitempty"`
	InternalPort int     `json:"internal_port,omitempty" yaml:"internal_port,omitempty"`
	Healthcheck  string  `json:"healthcheck,omitempty" yaml:"healthcheck,omitempty"`
	Sandbox      Sandbox `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
	Harness      string  `json:"harness,omitempty" yaml:"harness,omitempty"`
	Auth         string  `json:"auth,omitempty" yaml:"auth,omitempty"`
	Egress       Egress  `json:"egress" yaml:"egress,omitempty"`
	Policy       Policy  `json:"policy" yaml:"policy,omitempty"`
	// Files are copied into the checkout at every start and ignored by git: what the
	// service needs to run but the repository does not carry (an .npmrc).
	Files []PlacedFile `json:"files,omitempty" yaml:"files,omitempty"`
	// AgentConfig replaces the ticket's agent configuration for this service's agent
	// (nil inherits it, empty means none): how two roles get two rule sets.
	AgentConfig []string `json:"agent_config,omitempty" yaml:"agent_config,omitempty"`
	// Model is passed to this service's agent; empty leaves the harness's default.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
}

// PlacedFile is a host file (From, absolute) copied to Path inside a service's checkout.
type PlacedFile struct {
	Path string `json:"path" yaml:"path"`
	From string `json:"from" yaml:"from"`
}

type Build struct {
	Dockerfile string `json:"dockerfile,omitempty" yaml:"dockerfile,omitempty"`
}

type Dependency struct {
	Name  string            `json:"name" yaml:"name"`
	Image string            `json:"image" yaml:"image"`
	Env   map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

type Docker struct {
	NetworkMode NetworkMode `json:"network_mode,omitempty" yaml:"network_mode,omitempty"`
}

type Egress struct {
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
}

// Policy fields left unset inherit from the next level up. A nil Gates inherits;
// an empty, non-nil Gates means "no gates", which is why the tags do not omit it.
type Policy struct {
	Gates []Gate     `json:"gates" yaml:"gates"`
	Push  PushPolicy `json:"push" yaml:"push,omitempty"`
}

type PushPolicy struct {
	Mode PushMode `json:"mode,omitempty" yaml:"mode,omitempty"`
	// MergeCommit lets merge mode create a merge commit when the base branch has moved on
	// at the remote; without it only a fast-forward is pushed. nil inherits.
	MergeCommit *bool `json:"merge_commit,omitempty" yaml:"merge_commit,omitempty"`
	// GitHubUser is the gh account pull requests are opened as, when gh is logged in to
	// several; empty means gh's active account.
	GitHubUser GitHubLogin `json:"github_user,omitempty" yaml:"github_user,omitempty"`
}

// GitHubLogin is a GitHub account name. It is checked when read because it is passed to
// gh as an argument: it can never look like an option.
type GitHubLogin string

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)

func (l *GitHubLogin) UnmarshalText(b []byte) error {
	if len(b) > 0 && !githubLogin.Match(b) {
		return fmt.Errorf("invalid GitHub login %q (letters, digits, - and _, starting with a letter or digit)", b)
	}
	*l = GitHubLogin(b)
	return nil
}
