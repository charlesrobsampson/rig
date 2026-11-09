// Package domain holds the types shared by config, state, policy, and the API:
// tickets, services, policies, and the enums whose string forms are fixed by
// docs/formats.md.
package domain

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
	Policy       Policy            `json:"policy" yaml:"policy,omitempty"`
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
}
