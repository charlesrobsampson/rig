// Package contract reads and writes a multi-service ticket's contract.md
// (docs/formats.md §11): the API shapes the services agreed on, the changes agents
// propose, and the log of what a human accepted. Agents only ever append proposals; rig
// edits the agreed section and the changelog when a human accepts one.
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Status string

const (
	Open     Status = "open"
	Accepted Status = "accepted"
	Rejected Status = "rejected"
)

// Proposal is one entry under "## Proposed changes".
type Proposal struct {
	ID       string `json:"id"`
	From     string `json:"from"` // the proposing service
	Status   Status `json:"status"`
	What     string `json:"what"`
	Why      string `json:"why"`
	Rejected string `json:"rejected,omitempty"` // the reason, for a rejected proposal
	Line     int    `json:"line"`
	end      int    // 0-based line index just after the proposal's last line
}

// Block is a machine-readable fenced block in the agreed section.
type Block struct {
	Kind string `json:"kind"` // "endpoint" or "schema"
	Body string `json:"body"`
	Line int    `json:"line"`
}

// Problem is something wrong with the file. Problems never hide the entries that are fine.
type Problem struct {
	Line    int    `json:"line"` // 1-based; 0 when it is about the whole file
	Message string `json:"message"`
}

// Doc is a parsed contract.md.
type Doc struct {
	Title     string     `json:"title,omitempty"`
	Agreed    string     `json:"agreed"` // the agreed section with comments removed, trimmed
	Blocks    []Block    `json:"blocks"`
	Proposals []Proposal `json:"proposals"`
	Changelog []string   `json:"changelog"`
	Problems  []Problem  `json:"problems"`

	// positions, as 0-based line indexes into the original text, for the edit functions
	lines                               []string
	agreedEnd, proposedEnd, changelogAt int // where each section's text ends (exclusive); -1 if absent
}

var (
	proposalRe = regexp.MustCompile(`^### (P-[a-z][a-z0-9-]*-\d+) · from ([a-z][a-z0-9-]*) · (open|accepted|rejected)$`)
	idRe       = regexp.MustCompile(`^P-([a-z][a-z0-9-]*)-(\d+)$`)
	fenceRe    = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([A-Za-z0-9_-]*)")
)

// Section names, exactly as in the file.
const (
	secAgreed    = "Agreed"
	secProposed  = "Proposed changes"
	secChangelog = "Changelog"
)

// Parse reads a contract. It never fails: a section or entry it cannot read becomes a
// Problem and the rest is still returned.
func Parse(md string) Doc {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	original := strings.Split(md, "\n")
	lines := blankComments(original)
	var d Doc
	d.lines, d.agreedEnd, d.proposedEnd, d.changelogAt = original, -1, -1, -1
	d.Blocks, d.Proposals, d.Changelog, d.Problems = []Block{}, []Proposal{}, []string{}, []Problem{}

	type span struct{ start, end int } // line indexes, end exclusive
	sections := map[string]span{}
	var order []string
	inFence := false
	for i, line := range lines {
		if fenceRe.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# ") && d.Title == "":
			d.Title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
		case strings.HasPrefix(line, "## "):
			name := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if len(order) > 0 {
				prev := sections[order[len(order)-1]]
				prev.end = i
				sections[order[len(order)-1]] = prev
			}
			if _, dup := sections[name]; dup {
				d.Problems = append(d.Problems, Problem{i + 1, fmt.Sprintf("the section %q appears twice; only the first is read", name)})
				order = append(order, name+"\x00dup")
				sections[name+"\x00dup"] = span{i + 1, len(lines)}
				continue
			}
			order = append(order, name)
			sections[name] = span{i + 1, len(lines)}
		}
	}
	if len(order) > 0 {
		last := sections[order[len(order)-1]]
		last.end = len(lines)
		sections[order[len(order)-1]] = last
	}
	for _, name := range []string{secAgreed, secProposed, secChangelog} {
		if _, ok := sections[name]; !ok {
			d.Problems = append(d.Problems, Problem{0, fmt.Sprintf("the section \"## %s\" is missing", name)})
		}
	}

	if s, ok := sections[secAgreed]; ok {
		d.agreedEnd = s.end
		d.Agreed = strings.TrimSpace(strings.Join(lines[s.start:s.end], "\n"))
		d.Blocks = parseBlocks(lines, s.start, s.end)
	}
	if s, ok := sections[secProposed]; ok {
		d.proposedEnd = s.end
		d.Proposals, d.Problems = parseProposals(lines, s.start, s.end, d.Problems)
	}
	if s, ok := sections[secChangelog]; ok {
		d.changelogAt = s.end
		for _, l := range lines[s.start:s.end] {
			if t := strings.TrimSpace(l); strings.HasPrefix(t, "- ") {
				d.Changelog = append(d.Changelog, strings.TrimSpace(strings.TrimPrefix(t, "- ")))
			}
		}
	}
	return d
}

// parseBlocks finds the fenced `endpoint` and `schema` blocks (other fences are skipped).
func parseBlocks(lines []string, start, end int) []Block {
	blocks := []Block{}
	for i := start; i < end; i++ {
		m := fenceRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		j := i + 1
		for j < end && !fenceRe.MatchString(lines[j]) {
			j++
		}
		if m[2] == "endpoint" || m[2] == "schema" {
			blocks = append(blocks, Block{Kind: m[2], Body: strings.Join(lines[i+1:j], "\n"), Line: i + 1})
		}
		i = j
	}
	return blocks
}

// blankComments empties HTML comments (the template's guidance) outside code fences,
// keeping the line count so reported line numbers stay right.
func blankComments(lines []string) []string {
	out := make([]string, len(lines))
	inFence, inComment := false, false
	for i, line := range lines {
		if !inComment && fenceRe.MatchString(line) {
			inFence = !inFence
			out[i] = line
			continue
		}
		if inFence {
			out[i] = line
			continue
		}
		var b strings.Builder
		rest := line
		for rest != "" {
			if inComment {
				end := strings.Index(rest, "-->")
				if end < 0 {
					rest = ""
					break
				}
				rest, inComment = rest[end+3:], false
				continue
			}
			start := strings.Index(rest, "<!--")
			if start < 0 {
				b.WriteString(rest)
				break
			}
			b.WriteString(rest[:start])
			rest, inComment = rest[start+4:], true
		}
		out[i] = b.String()
	}
	return out
}

func parseProposals(lines []string, start, end int, problems []Problem) ([]Proposal, []Problem) {
	proposals := []Proposal{}
	seen := map[string]int{}
	inFence := false

	type entry struct {
		line    int
		heading string
		body    []string
		end     int
	}
	var entries []entry
	for i := start; i < end; i++ {
		line := lines[i]
		if fenceRe.MatchString(line) {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "###") {
			entries = append(entries, entry{line: i + 1, heading: strings.TrimRight(line, " \t")})
			continue
		}
		if len(entries) > 0 {
			entries[len(entries)-1].body = append(entries[len(entries)-1].body, line)
		}
	}
	for i := range entries {
		entries[i].end = entries[i].line + len(entries[i].body) // line is 1-based: index of the heading + 1 + body length
	}

	for _, e := range entries {
		m := proposalRe.FindStringSubmatch(e.heading)
		if m == nil {
			problems = append(problems, Problem{e.line, fmt.Sprintf("%q is not a proposal heading; write `### P-<service>-<n> · from <service> · open`", e.heading)})
			continue
		}
		p := Proposal{ID: m[1], From: m[2], Status: Status(m[3]), Line: e.line, end: e.end}
		var bad []string
		if id := idRe.FindStringSubmatch(p.ID); id != nil && id[1] != p.From {
			bad = append(bad, fmt.Sprintf("the id %s names service %q but the proposal says it is from %q", p.ID, id[1], p.From))
		}
		if first, dup := seen[p.ID]; dup {
			bad = append(bad, fmt.Sprintf("the id %s was already used on line %d", p.ID, first))
		}
		p.What, p.Why, p.Rejected = fields(e.body)
		if p.What == "" {
			bad = append(bad, "it has no **What:** line")
		}
		if p.Why == "" {
			bad = append(bad, "it has no **Why:** line")
		}
		if p.Status == Rejected && p.Rejected == "" {
			bad = append(bad, "a rejected proposal needs a **Rejected:** reason")
		}
		if len(bad) > 0 {
			for _, msg := range bad {
				problems = append(problems, Problem{e.line, fmt.Sprintf("proposal %s: %s", p.ID, msg)})
			}
			if _, dup := seen[p.ID]; !dup {
				seen[p.ID] = e.line // a malformed first use still reserves the id
			}
			continue
		}
		seen[p.ID] = e.line
		proposals = append(proposals, p)
	}
	return proposals, problems
}

// fields reads the **What:**, **Why:** and **Rejected:** values of a proposal; a value
// continues over the following lines until the next marker.
func fields(body []string) (what, why, rejected string) {
	var current *string
	set := map[string]*string{"**What:**": &what, "**Why:**": &why, "**Rejected:**": &rejected}
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		matched := false
		for marker, target := range set {
			if strings.HasPrefix(trimmed, marker) {
				current, matched = target, true
				*current = strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
				break
			}
		}
		if !matched && current != nil {
			*current += "\n" + strings.TrimRight(line, " \t")
		}
	}
	return strings.TrimSpace(what), strings.TrimSpace(why), strings.TrimSpace(rejected)
}

// AgreedHash identifies the agreed section's content (comments and surrounding
// whitespace do not count), so a change to it can be told from any other edit.
func (d Doc) AgreedHash() string {
	sum := sha256.Sum256([]byte(d.Agreed))
	return hex.EncodeToString(sum[:])
}

// OpenProposals are the proposals still waiting for a human.
func (d Doc) OpenProposals() []Proposal {
	var out []Proposal
	for _, p := range d.Proposals {
		if p.Status == Open {
			out = append(out, p)
		}
	}
	return out
}

// AgreedEmpty is true while nothing has been agreed: the section holds only the
// template's guidance comment or whitespace.
func (d Doc) AgreedEmpty() bool { return d.Agreed == "" }

// Proposal finds a proposal by id.
func (d Doc) Proposal(id string) (Proposal, bool) {
	for _, p := range d.Proposals {
		if p.ID == id {
			return p, true
		}
	}
	return Proposal{}, false
}

// NextProposalID is the id the next proposal of a service should use: P-<service>-<n>
// with n one more than the highest the service has used, so ids are stable and agents
// never collide. Malformed entries still reserve their number.
func NextProposalID(md, service string) string {
	highest := 0
	for _, line := range strings.Split(md, "\n") {
		if m := proposalRe.FindStringSubmatch(strings.TrimRight(line, " \t\r")); m != nil && m[2] == service {
			if id := idRe.FindStringSubmatch(m[1]); id != nil {
				if n, _ := strconv.Atoi(id[2]); n > highest {
					highest = n
				}
			}
		}
	}
	return fmt.Sprintf("P-%s-%d", service, highest+1)
}

// Template is the contract.md `rig new` writes for a ticket with several services.
func Template(ticketID string) string {
	return fmt.Sprintf(`# Contract: %s

## Agreed
<!-- What the services have agreed on, as the human approved it. Write the API shapes
here (before the plan is approved) as fenced blocks tagged endpoint or schema; plain
markdown is fine around them. Agents never edit this section: to change it, add a
proposal below. -->

## Proposed changes
<!-- Agents append entries here, one per change:

### P-<service>-<n> · from <service> · open
**What:** the change.
**Why:** the reason.

The number is that service's next integer. A human accepts or rejects each one. -->

## Changelog
`, ticketID)
}
