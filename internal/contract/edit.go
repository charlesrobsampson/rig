package contract

import (
	"errors"
	"fmt"
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
