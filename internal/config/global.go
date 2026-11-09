package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/charlesrobsampson/rig/internal/domain"
)

type Global struct {
	Summarizer Summarizer `json:"summarizer"`
	Defaults   Defaults   `json:"defaults"`
	Limits     Limits     `json:"limits"`
	Paths      Paths      `json:"paths"`
	Auth       Auth       `json:"auth"`
	Secrets    Secrets    `json:"secrets"`
	Docker     Docker     `json:"docker"`
	Egress     Egress     `json:"egress"`
}

type Summarizer struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

type Defaults struct {
	Harness         string             `json:"harness"`
	Sandbox         domain.Sandbox     `json:"sandbox"`
	TerminalBackend string             `json:"terminal_backend"`
	TicketSource    string             `json:"ticket_source"`
	NetworkMode     domain.NetworkMode `json:"network_mode"`
	// BranchPrefix starts the default branch name of new tickets: <prefix><ticket>/<service>.
	BranchPrefix string        `json:"branch_prefix"`
	Policy       domain.Policy `json:"policy"`
}

type Limits struct {
	MaxActiveTickets int `json:"max_active_tickets"`
}

type Paths struct {
	Home string `json:"home"`
}

type Auth struct {
	Default  string                 `json:"default,omitempty"`
	Profiles map[string]AuthProfile `json:"profiles,omitempty"`
}

// AuthProfile names the secrets it needs; their values come from the secrets
// resolver and never appear in config.
type AuthProfile struct {
	Type    domain.AuthType   `json:"type"`
	Secrets []string          `json:"secrets,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Secrets says where secret values come from: the macOS Keychain (generic passwords of
// service KeychainService, account = the secret's name) first, then the EnvFile.
type Secrets struct {
	KeychainService string `json:"keychain_service"`
	EnvFile         string `json:"env_file,omitempty"`
}

type Docker struct {
	ExtraCACerts []string `json:"extra_ca_certs,omitempty"`
	// PortRange is where host ports for service containers are allocated.
	PortRange PortRange `json:"port_range"`
	// AgentBase is the image an agent runs on when its service has no Dockerfile.
	AgentBase string `json:"agent_base"`
	// ClaudeVersion is what the agent image's installer is asked for ("latest" by default).
	ClaudeVersion string `json:"claude_version"`
}

type PortRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type Egress struct {
	DefaultAllow []string `json:"default_allow"`
	DefaultPorts []int    `json:"default_ports"`
}

// Locations under the rig home (docs/formats.md §2).
func (g Global) SocketPath() string { return filepath.Join(g.Paths.Home, "rigd.sock") }
func (g Global) PIDPath() string    { return filepath.Join(g.Paths.Home, "rigd.pid") }
func (g Global) LogPath() string    { return filepath.Join(g.Paths.Home, "rigd.log") }
func (g Global) StorePath() string  { return filepath.Join(g.Paths.Home, "store.json") }
func (g Global) TicketsDir() string { return filepath.Join(g.Paths.Home, "tickets") }

type GlobalOptions struct {
	// Path is the config file; empty means $RIG_CONFIG, then ~/.config/rig/config.json.
	Path     string
	Env      func(string) string
	UserHome func() (string, error)
}

var (
	branchPrefix = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*/)*$`)
	hostPattern  = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	terminals    = []string{"herdr", "tmux"}
	summarizers  = []string{"none", "ollama", "claude"}
)

func defaultGlobal(userHome string) Global {
	return Global{
		Summarizer: Summarizer{Provider: "none", Endpoint: "http://localhost:11434"},
		Defaults: Defaults{
			Harness:         "claude-code",
			Sandbox:         domain.SandboxContainer,
			TerminalBackend: "herdr",
			TicketSource:    "manual",
			NetworkMode:     domain.NetworkBridge,
			BranchPrefix:    "rig/",
			Policy: domain.Policy{
				Gates: []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval},
				Push:  domain.PushPolicy{Mode: domain.PushPR},
			},
		},
		Limits:  Limits{MaxActiveTickets: 8},
		Paths:   Paths{Home: filepath.Join(userHome, ".rig")},
		Egress:  Egress{DefaultAllow: []string{"api.anthropic.com"}, DefaultPorts: []int{443}},
		Secrets: Secrets{KeychainService: "rig"},
		Docker:  Docker{PortRange: PortRange{Min: 41000, Max: 41999}, AgentBase: "debian:bookworm-slim", ClaudeVersion: "latest"},
	}
}

// LoadGlobal reads the config over built-in defaults. A missing file is fine when it
// was not asked for by name; unknown keys and invalid values are errors.
func LoadGlobal(opts GlobalOptions) (Global, error) {
	getenv := opts.Env
	if getenv == nil {
		getenv = os.Getenv
	}
	userHome := opts.UserHome
	if userHome == nil {
		userHome = os.UserHomeDir
	}
	home, err := userHome()
	if err != nil {
		return Global{}, fmt.Errorf("find home directory: %w", err)
	}

	path, explicit := opts.Path, opts.Path != ""
	if !explicit {
		if path = getenv("RIG_CONFIG"); path != "" {
			explicit = true
		} else {
			path = filepath.Join(home, ".config", "rig", "config.json")
		}
	}

	g := defaultGlobal(home)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := decodeStrict(data, &g); err != nil {
			return Global{}, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist) && !explicit:
	default:
		return Global{}, fmt.Errorf("read config: %w", err)
	}

	if rigHome := getenv("RIG_HOME"); rigHome != "" {
		g.Paths.Home = rigHome
	}
	g.Paths.Home = expandHome(g.Paths.Home, home)
	g.Secrets.EnvFile = expandHome(g.Secrets.EnvFile, home)
	for i, c := range g.Docker.ExtraCACerts {
		g.Docker.ExtraCACerts[i] = expandHome(c, home)
	}

	if problems := validateGlobal(g); len(problems) > 0 {
		return Global{}, fmt.Errorf("invalid config %s:\n  - %s", path, strings.Join(problems, "\n  - "))
	}
	return g, nil
}

func decodeStrict(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return fmt.Errorf("%w (line %d)", err, lineOf(data, syntax.Offset))
		case errors.As(err, &typeErr):
			return fmt.Errorf("%s: expected %s, got %s (line %d)", typeErr.Field, typeErr.Type, typeErr.Value, lineOf(data, typeErr.Offset))
		}
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the config object")
	}
	return nil
}

func lineOf(data []byte, offset int64) int {
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	return bytes.Count(data[:offset], []byte("\n")) + 1
}

func expandHome(p, home string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	}
	return p
}

func validateGlobal(g Global) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if g.Defaults.Harness == "" {
		add("defaults.harness: required")
	}
	if g.Defaults.TicketSource == "" {
		add("defaults.ticket_source: required")
	}
	if !contains(terminals, g.Defaults.TerminalBackend) {
		add("defaults.terminal_backend: %q is not supported (want %s)", g.Defaults.TerminalBackend, strings.Join(terminals, " or "))
	}
	if !contains(summarizers, g.Summarizer.Provider) {
		add("summarizer.provider: %q is not supported (want %s)", g.Summarizer.Provider, strings.Join(summarizers, ", "))
	}
	if g.Summarizer.Provider == "ollama" && strings.TrimSpace(g.Summarizer.Model) == "" {
		add("summarizer.model: required when the provider is ollama (for example \"llama3:8b\")")
	}
	if !branchPrefix.MatchString(g.Defaults.BranchPrefix) {
		add("defaults.branch_prefix: %q must be empty or end with a slash, like \"rig/\" (letters, digits, dots, dashes, underscores)", g.Defaults.BranchPrefix)
	}
	if g.Limits.MaxActiveTickets < 1 {
		add("limits.max_active_tickets: must be at least 1")
	}
	if !filepath.IsAbs(g.Paths.Home) {
		add("paths.home: %s must be an absolute path", g.Paths.Home)
	}
	for i, c := range g.Docker.ExtraCACerts {
		if !filepath.IsAbs(c) {
			add("docker.extra_ca_certs[%d]: %s must be an absolute path", i, c)
		}
	}
	for i, h := range g.Egress.DefaultAllow {
		if !hostPattern.MatchString(h) {
			add("egress.default_allow[%d]: %q is not a hostname or *.suffix pattern", i, h)
		}
	}
	for i, port := range g.Egress.DefaultPorts {
		if port < 1 || port > 65535 {
			add("egress.default_ports[%d]: %d must be between 1 and 65535", i, port)
		}
	}
	if r := g.Docker.PortRange; r.Min < 1024 || r.Max > 65535 || r.Min > r.Max {
		add("docker.port_range: %d-%d must be within 1024-65535 with min <= max", r.Min, r.Max)
	}
	if g.Secrets.KeychainService == "" {
		add("secrets.keychain_service: required")
	}
	if g.Secrets.EnvFile != "" && !filepath.IsAbs(g.Secrets.EnvFile) {
		add("secrets.env_file: %q must be an absolute path", g.Secrets.EnvFile)
	}
	if g.Docker.AgentBase == "" {
		add("docker.agent_base: an image name is required")
	}
	if g.Docker.ClaudeVersion == "" {
		add("docker.claude_version: required (use \"latest\" for the newest)")
	}
	return append(problems, validateAuth(g.Auth)...)
}

func validateAuth(a Auth) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	names := make([]string, 0, len(a.Profiles))
	for name := range a.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	if a.Default != "" {
		if _, ok := a.Profiles[a.Default]; !ok {
			add("auth.default: no profile named %q", a.Default)
		}
	}
	for _, name := range names {
		p := a.Profiles[name]
		prefix := "auth.profiles." + name
		if err := domain.ValidateName(name); err != nil {
			add("%s: %v", prefix, err)
		}
		switch p.Type {
		case "":
			add("%s.type: required", prefix)
		case domain.AuthHostLogin:
			if len(p.Secrets) > 0 {
				add("%s.secrets: host_login profiles use the machine's own login and take no secrets", prefix)
			}
		case domain.AuthOAuthToken, domain.AuthAPIKey, domain.AuthAuthToken:
			if len(p.Secrets) == 0 {
				add("%s.secrets: %s profiles need at least one secret name", prefix, p.Type)
			}
		}
		problems = append(problems, checkSecrets(prefix+".secrets", p.Secrets)...)
		problems = append(problems, checkEnv(prefix+".env", p.Env)...)
	}
	return problems
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
