package contract

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	// ErrNoProposal: the contract has no valid proposal with that id.
	ErrNoProposal = errors.New("no such proposal")
	// ErrNotOpen: the proposal was already accepted or rejected.
	ErrNotOpen = errors.New("proposal is not open")
)

// Accept returns the contract text after a human accepted a proposal: its heading says
// accepted, its change is merged into the agreed section, and the changelog records it.
// The change is added to the agreed section as the proposer wrote it (including any
// fenced endpoint or schema blocks), under a heading naming the proposal; a human may
// tidy the section afterwards. Only the text is returned: nothing is written.
func Accept(md, id string, now time.Time) (string, error) {
	d, p, err := open(md, id)
	if err != nil {
		return "", err
	}
	lines := append([]string(nil), d.lines...)
	lines[p.Line-1] = statusHeading(lines[p.Line-1], Accepted)

	merged := []string{"", fmt.Sprintf("#### %s (accepted %s): %s", p.ID, now.Format("2006-01-02"), firstLine(p.What)), ""}
	merged = append(merged, strings.Split(p.What, "\n")...)
	at := d.agreedEnd
	for at > 0 && strings.TrimSpace(lines[at-1]) == "" { // right after the section's last text
		at--
	}
	lines = insertAt(lines, at, merged)
	return appendChangelog(lines, now, fmt.Sprintf("%s accepted by human: %s", p.ID, firstLine(p.What))), nil
}

// Reject returns the contract text after a human rejected a proposal with a reason: its
// heading says rejected, it gets a **Rejected:** line, and the changelog records it.
func Reject(md, id, reason string, now time.Time) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", errors.New("a rejection needs a reason")
	}
	d, p, err := open(md, id)
	if err != nil {
		return "", err
	}
	lines := append([]string(nil), d.lines...)
	lines[p.Line-1] = statusHeading(lines[p.Line-1], Rejected)
	end := p.end
	for end > p.Line && strings.TrimSpace(lines[end-1]) == "" {
		end-- // keep the reason next to the proposal's own text, not after its trailing blank lines
	}
	lines = insertAt(lines, end, []string{"**Rejected:** " + strings.Join(strings.Fields(reason), " ")})
	return appendChangelog(lines, now, fmt.Sprintf("%s rejected by human: %s", p.ID, strings.Join(strings.Fields(reason), " "))), nil
}

func open(md, id string) (Doc, Proposal, error) {
	d := Parse(md)
	p, ok := d.Proposal(id)
	if !ok {
		return d, p, fmt.Errorf("%w: %s", ErrNoProposal, id)
	}
	if p.Status != Open {
		return d, p, fmt.Errorf("%w: %s is already %s", ErrNotOpen, id, p.Status)
	}
	if d.agreedEnd < 0 {
		return d, p, errors.New("the contract has no \"## Agreed\" section to merge the change into")
	}
	return d, p, nil
}

func statusHeading(heading string, to Status) string {
	i := strings.LastIndex(heading, " · ")
	return heading[:i] + " · " + string(to)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// insertAt puts add into lines before index at.
func insertAt(lines []string, at int, add []string) []string {
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	return append(out, lines[at:]...)
}

// appendChangelog adds a dated line to the changelog, creating the section if needed.
func appendChangelog(lines []string, now time.Time, entry string) string {
	line := fmt.Sprintf("- %s %s", now.Format("2006-01-02"), entry)
	d := Parse(strings.Join(lines, "\n"))
	if d.changelogAt < 0 {
		lines = append(trimTrailingBlank(lines), "", "## Changelog", line, "")
		return strings.Join(lines, "\n")
	}
	at := d.changelogAt
	for at > 0 && strings.TrimSpace(lines[at-1]) == "" { // after the last entry, before trailing blank lines
		at--
	}
	lines = insertAt(lines, at, []string{line})
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

var serviceNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ErrNoProposalSection: the contract has nowhere to put a proposal.
var ErrNoProposalSection = errors.New(`the contract has no "## Proposed changes" section`)

// Propose returns the contract text with a new open proposal from service appended at
// the end of "## Proposed changes", and the id it was given. It never touches "## Agreed".
// what and why are single paragraphs or several lines; why may be empty only if the
// caller has a reason to allow it (the parser requires both, so Propose does too).
func Propose(md, service, what, why string) (id, out string, err error) {
	what, why = strings.TrimSpace(what), strings.TrimSpace(why)
	if !serviceNameRe.MatchString(service) {
		return "", "", fmt.Errorf("%q is not a service name (lowercase letters, digits and dashes)", service)
	}
	if what == "" || why == "" {
		return "", "", errors.New("a proposal needs both what to change and why")
	}
	for _, text := range []string{what, why} {
		for _, line := range strings.Split(text, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "**What:**") || strings.HasPrefix(l, "**Why:**") || strings.HasPrefix(l, "**Rejected:**") || strings.HasPrefix(l, "### ") || strings.HasPrefix(l, "## ") {
				return "", "", fmt.Errorf("a proposal's text cannot contain a line starting %q: it would be read as part of the file's structure", strings.Fields(l)[0])
			}
		}
	}
	d := Parse(md)
	if d.proposedEnd < 0 {
		return "", "", ErrNoProposalSection
	}
	id = NextProposalID(md, service)
	block := []string{fmt.Sprintf("### %s · from %s · open", id, service)}
	block = append(block, "**What:** "+strings.ReplaceAll(what, "\r\n", "\n"))
	block = append(block, "**Why:** "+strings.ReplaceAll(why, "\r\n", "\n"))

	lines := append([]string(nil), d.lines...)
	at := d.proposedEnd
	for at > 0 && strings.TrimSpace(lines[at-1]) == "" { // right after the section's last text
		at--
	}
	// a blank line before the new proposal; the blank lines that followed the last one
	// stay after it, so the next section keeps its spacing
	insert := append([]string{""}, strings.Split(strings.Join(block, "\n"), "\n")...)
	lines = insertAt(lines, at, insert)
	out = strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return id, out, nil
}
