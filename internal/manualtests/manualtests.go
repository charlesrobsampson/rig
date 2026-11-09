// Package manualtests reads a ticket's manual-tests.md (docs/formats.md §12): steps for
// a human to check the result by hand, written by the agent with placeholders for
// addresses that only the running ticket knows (its host ports). Rendering fills them in
// from live state, so the same file works for every ticket and every restart.
package manualtests

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Step is one `### <id>: <title>` section and the text under it.
type Step struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Doc is a parsed file: the text before the first step, and the steps.
type Doc struct {
	Preamble string `json:"preamble"`
	Steps    []Step `json:"steps"`
}

var (
	headingRe = regexp.MustCompile(`^###\s+(\S+?):\s*(.*\S)\s*$`)
	idRe      = regexp.MustCompile(`^[a-z0-9-]+$`)
	fenceRe   = regexp.MustCompile("^\\s*(```|~~~)")
)

// Parse splits the file into steps. A heading that is not a valid step (no id, a bad id
// such as one with capitals or underscores, a repeated id) is reported, all at once with
// line numbers; headings inside code fences are text, not steps.
func Parse(md string) (Doc, error) {
	var doc Doc
	var problems []string
	seen := map[string]int{}
	var current *Step
	var body []string
	inFence := false
	flush := func() {
		if current != nil {
			current.Body = strings.TrimSpace(strings.Join(body, "\n"))
			doc.Steps = append(doc.Steps, *current)
		} else {
			doc.Preamble = strings.TrimSpace(strings.Join(body, "\n"))
		}
		body = nil
	}

	for i, line := range strings.Split(md, "\n") {
		if fenceRe.MatchString(line) {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "### ") || !inFence && line == "###" {
			flush()
			current = nil
			m := headingRe.FindStringSubmatch(line)
			switch {
			case m == nil:
				problems = append(problems, fmt.Sprintf("line %d: %q is not a step heading; write `### <id>: <title>`", i+1, line))
				current = &Step{} // keep its text out of the previous step
				current.ID = ""
			case !idRe.MatchString(m[1]):
				problems = append(problems, fmt.Sprintf("line %d: step id %q must be lowercase letters, digits and dashes", i+1, m[1]))
				current = &Step{ID: m[1], Title: m[2]}
			case seen[m[1]] != 0:
				problems = append(problems, fmt.Sprintf("line %d: step id %q was already used on line %d", i+1, m[1], seen[m[1]]))
				current = &Step{ID: m[1], Title: m[2]}
			default:
				seen[m[1]] = i + 1
				current = &Step{ID: m[1], Title: m[2]}
			}
			continue
		}
		body = append(body, line)
	}
	flush()
	if len(problems) > 0 {
		return Doc{}, &Problems{Problems: problems}
	}
	return doc, nil
}

// Problems is an error carrying every problem found, so a fix needs one round trip.
type Problems struct{ Problems []string }

func (p *Problems) Error() string {
	return fmt.Sprintf("manual-tests.md has %d problem(s): %s", len(p.Problems), strings.Join(p.Problems, "; "))
}

// Vars is what placeholders resolve against.
type Vars struct {
	TicketID string
	Services map[string]ServiceVars
	// Dependencies is the set of dependency names (their hostname on the ticket network
	// is their name).
	Dependencies map[string]bool
}

// ServiceVars are one service's addresses. HostPort is 0 when the service is not running
// (or publishes no port); InternalPort is the port inside the ticket's network.
type ServiceVars struct {
	HostPort     int
	InternalPort int
	// NotRunning explains why HostPort is unavailable ("stopped", "provisioned"...).
	NotRunning string
}

var placeholderRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// Render parses the file and fills every placeholder. Unknown placeholders and
// placeholders for a service that is not running are all collected and returned
// together (as *Problems) instead of leaving some unfilled.
func Render(md string, vars Vars) (Doc, error) {
	doc, err := Parse(md)
	if err != nil {
		return Doc{}, err
	}
	var problems []string
	fill := func(text string, firstLine int, owner string) string {
		return placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
			name := strings.TrimSpace(placeholderRe.FindStringSubmatch(m)[1])
			value, problem := resolve(name, vars)
			if problem != "" {
				problems = append(problems, fmt.Sprintf("%s: {{%s}}: %s", owner, name, problem))
				return m
			}
			return value
		})
	}
	doc.Preamble = fill(doc.Preamble, 1, "the introduction")
	for i := range doc.Steps {
		doc.Steps[i].Title = fill(doc.Steps[i].Title, 0, "step "+doc.Steps[i].ID)
		doc.Steps[i].Body = fill(doc.Steps[i].Body, 0, "step "+doc.Steps[i].ID)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Doc{}, &Problems{Problems: unique(problems)}
	}
	return doc, nil
}

func unique(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func resolve(name string, v Vars) (value, problem string) {
	if name == "ticket.id" {
		return v.TicketID, ""
	}
	owner, field, ok := strings.Cut(name, ".")
	if !ok || owner == "" || field == "" {
		return "", "not a placeholder; use {{<service>.url}}, {{<service>.port}}, {{<service>.internal_url}}, {{<dependency>.host}} or {{ticket.id}}"
	}
	if svc, isService := v.Services[owner]; isService {
		switch field {
		case "url", "port":
			if svc.HostPort == 0 {
				return "", fmt.Sprintf("service %s has no host port right now (%s); start the ticket first", owner, orDefault(svc.NotRunning, "it publishes none"))
			}
			if field == "port" {
				return fmt.Sprint(svc.HostPort), ""
			}
			return fmt.Sprintf("http://localhost:%d", svc.HostPort), ""
		case "internal_url":
			if svc.InternalPort == 0 {
				return "", fmt.Sprintf("service %s has no internal_port", owner)
			}
			return fmt.Sprintf("http://%s:%d", owner, svc.InternalPort), ""
		}
		return "", fmt.Sprintf("service %s has no %q (try url, port or internal_url)", owner, field)
	}
	if v.Dependencies[owner] {
		if field == "host" {
			return owner, ""
		}
		return "", fmt.Sprintf("dependency %s has no %q (try host)", owner, field)
	}
	return "", fmt.Sprintf("%q is neither a service nor a dependency of this ticket", owner)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
