package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/charlesrobsampson/rig/internal/domain"
)

type TicketOptions struct {
	// RepoCheck reports whether a service repository is usable; nil requires an
	// existing directory. Tests inject their own.
	RepoCheck func(path string) error
}

var envVarName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func LoadTicket(path string, opts TicketOptions) (domain.Ticket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Ticket{}, fmt.Errorf("read ticket: %w", err)
	}
	return ParseTicket(data, opts)
}

// ParseTicket decodes ticket.yaml strictly (unknown keys are errors naming the key
// and line), applies defaults, and reports every validation problem at once.
func ParseTicket(data []byte, opts TicketOptions) (domain.Ticket, error) {
	var t domain.Ticket
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		if errors.Is(err, io.EOF) {
			return domain.Ticket{}, errors.New("ticket.yaml is empty")
		}
		return domain.Ticket{}, fmt.Errorf("parse ticket.yaml: %w", err)
	}
	repoCheck := opts.RepoCheck
	if repoCheck == nil {
		repoCheck = requireDirectory
	}
	if problems := validateTicket(&t, repoCheck); len(problems) > 0 {
		return domain.Ticket{}, fmt.Errorf("invalid ticket.yaml:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return t, nil
}

func requireDirectory(path string) error {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return errors.New("not an existing directory")
	}
	return nil
}

func validateTicket(t *domain.Ticket, repoCheck func(string) error) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if t.ID == "" {
		add("id: required")
	} else if err := domain.ValidateTicketID(t.ID); err != nil {
		add("%v", err)
	}
	if t.Title == "" {
		add("title: required")
	}
	if t.Source.Type == "" {
		t.Source.Type = "manual"
	}
	if len(t.Services) == 0 {
		add("services: at least one service is required")
	}

	for i := range t.Services {
		problems = append(problems, validateService(i, &t.Services[i], repoCheck)...)
	}
	problems = append(problems, checkServiceOverlap(t.Services)...)

	for i, d := range t.Dependencies {
		if err := domain.ValidateName(d.Name); err != nil {
			add("dependencies[%d].name: %v", i, err)
		}
		if d.Image == "" {
			add("dependencies[%d].image: required", i)
		}
		problems = append(problems, checkEnv(fmt.Sprintf("dependencies[%d].env", i), d.Env)...)
	}
	if err := domain.CheckUniqueNames(t.Services, t.Dependencies); err != nil {
		add("%v", err)
	}

	problems = append(problems, checkEnv("env", t.Env)...)
	problems = append(problems, checkSecrets("secrets", t.Secrets)...)
	for i, p := range t.AgentConfig {
		if !filepath.IsAbs(p) {
			add("agent_config[%d]: %s must be an absolute path", i, p)
		}
	}
	if t.Review != nil {
		problems = append(problems, validateReview(t)...)
	}
	return problems
}

func validateService(i int, s *domain.Service, repoCheck func(string) error) []string {
	var problems []string
	field := func(name, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("services[%d].%s: %s", i, name, fmt.Sprintf(format, args...)))
	}

	if s.Name == "" {
		field("name", "required")
	} else if err := domain.ValidateName(s.Name); err != nil {
		field("name", "%v", err)
	}
	switch {
	case s.Repo == "":
		field("repo", "required")
	case !filepath.IsAbs(s.Repo):
		field("repo", "%s must be an absolute path", s.Repo)
	default:
		if err := repoCheck(s.Repo); err != nil {
			field("repo", "%s: %v", s.Repo, err)
		}
	}
	if s.Branch == "" {
		field("branch", "required")
	}

	if s.Path == "" && s.Name != "" {
		s.Path = "./" + s.Name
	}
	if filepath.IsAbs(s.Path) {
		field("path", "%s must be relative to the ticket directory", s.Path)
	} else if clean := filepath.Clean(s.Path); s.Path != "" && (clean == "." || clean == ".." || strings.HasPrefix(clean, "../")) {
		field("path", "%s must stay inside the ticket directory", s.Path)
	}
	placed := map[string]bool{}
	for j, f := range s.Files {
		name := fmt.Sprintf("files[%d]", j)
		clean := filepath.Clean(f.Path)
		switch {
		case f.Path == "" || filepath.IsAbs(f.Path) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../"):
			field(name+".path", "%q must be a relative path that stays inside the checkout", f.Path)
		case clean == ".git" || strings.HasPrefix(clean, ".git/"):
			field(name+".path", "%q is inside .git", f.Path)
		case placed[clean]:
			field(name+".path", "%q is placed twice", f.Path)
		}
		placed[clean] = true
		if !filepath.IsAbs(f.From) {
			field(name+".from", "%q must be an absolute path", f.From)
		}
	}
	for j, p := range s.AgentConfig {
		if !filepath.IsAbs(p) {
			field(fmt.Sprintf("agent_config[%d]", j), "%s must be an absolute path", p)
		}
	}
	if s.InternalPort != 0 && (s.InternalPort < 1 || s.InternalPort > 65535) {
		field("internal_port", "%d must be between 1 and 65535", s.InternalPort)
	}
	return problems
}

// checkServiceOverlap catches services that would collide on disk or on a branch.
// Container ports are deliberately not compared: each service has its own container,
// and host ports are allocated at run time.
func checkServiceOverlap(services []domain.Service) []string {
	var problems []string
	for j := range services {
		for i := 0; i < j; i++ {
			a, b := filepath.Clean(services[i].Path), filepath.Clean(services[j].Path)
			switch {
			case services[i].Path == "" || services[j].Path == "":
			case a == b:
				problems = append(problems, fmt.Sprintf("services[%d].path: %s is already used by services[%d]", j, services[j].Path, i))
			case strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/"):
				problems = append(problems, fmt.Sprintf("services[%d].path: %s is inside (or contains) services[%d].path %s", j, services[j].Path, i, services[i].Path))
			}
			if services[i].Repo != "" && filepath.Clean(services[i].Repo) == filepath.Clean(services[j].Repo) &&
				services[i].Branch != "" && services[i].Branch == services[j].Branch {
				problems = append(problems, fmt.Sprintf("services[%d].branch: services[%d] already uses branch %s of %s", j, i, services[j].Branch, services[j].Repo))
			}
		}
	}
	return problems
}

func checkEnv(prefix string, env map[string]string) []string {
	var problems []string
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if looksLikeSecret(env[k]) {
			problems = append(problems, fmt.Sprintf("%s.%s: value looks like a secret; list %s under secrets: and keep the value in the keychain", prefix, k, k))
		}
	}
	return problems
}

// checkSecrets never echoes anything after "=" or an entry that looks like a secret:
// a mistaken NAME=value or pasted token is probably real.
func checkSecrets(prefix string, secrets []string) []string {
	var problems []string
	var seen []string
	for _, s := range secrets {
		name, _, hasValue := strings.Cut(s, "=")
		switch {
		case hasValue:
			problems = append(problems, fmt.Sprintf("%s: entry %q has a value; list names only, never values", prefix, name))
		case !envVarName.MatchString(s) && looksLikeSecret(s):
			problems = append(problems, fmt.Sprintf("%s: an entry is not an environment variable name and looks like a secret value; list names only, never values", prefix))
		case !envVarName.MatchString(s):
			problems = append(problems, fmt.Sprintf("%s: %q is not a valid environment variable name", prefix, s))
		case slices.Contains(seen, s):
			problems = append(problems, fmt.Sprintf("%s: duplicate %q", prefix, s))
		}
		seen = append(seen, s)
	}
	return problems
}

func validateReview(t *domain.Ticket) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, "review."+fmt.Sprintf(format, args...)) }
	r := t.Review
	if r.Of == "" && len(t.Services) == 1 {
		r.Of = t.Services[0].Name
	}
	found := false
	for _, s := range t.Services {
		found = found || s.Name == r.Of
		if s.Name == domain.TesterService && r.Tester != nil {
			problems = append(problems, fmt.Sprintf("services: %q is the tester's name in a ticket with a review.tester; rename the service", s.Name))
		}
		if s.Name == domain.ReviewerService {
			problems = append(problems, fmt.Sprintf("services: %q is the reviewer's name in a ticket with a review; rename the service", s.Name))
		}
	}
	switch {
	case r.Of == "":
		add("of: required when the ticket has several services")
	case !found:
		add("of: %q is not a service of this ticket", r.Of)
	}
	for i, p := range r.AgentConfig {
		if !filepath.IsAbs(p) {
			add("agent_config[%d]: %s must be an absolute path", i, p)
		}
	}
	for i, p := range r.Copy {
		clean := filepath.Clean(p)
		switch {
		case p == "" || filepath.IsAbs(p) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../"):
			add("copy[%d]: %q must be a relative path that stays inside the checkout", i, p)
		case clean == ".git" || strings.HasPrefix(clean, ".git/"):
			add("copy[%d]: %q is inside .git", i, p)
		}
	}
	if r.Tester != nil {
		for i, p := range r.Tester.AgentConfig {
			if !filepath.IsAbs(p) {
				add("tester.agent_config[%d]: %s must be an absolute path", i, p)
			}
		}
		if r.Tester.Brief != "" && !filepath.IsAbs(r.Tester.Brief) {
			add("tester.brief: %s must be an absolute path", r.Tester.Brief)
		}
	}
	if r.MaxRounds == 0 {
		r.MaxRounds = 3
	} else if r.MaxRounds < 0 {
		add("max_rounds: %d must be at least 1", r.MaxRounds)
	}
	if r.BudgetUSD < 0 {
		add("budget_usd: %v must not be negative", r.BudgetUSD)
	}
	return problems
}
