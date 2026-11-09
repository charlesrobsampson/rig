package tui

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/charlesrobsampson/rig/internal/api"
)

// ErrMissing is what a Source returns for a file or section that does not exist (yet),
// as opposed to one that failed to load.
var ErrMissing = errors.New("does not exist")

// Tab is one page of a ticket's detail view.
type Tab int

const (
	TabPlan Tab = iota
	TabTasks
	TabContract
	TabDiff
	TabLogs
	TabManual
	tabCount
)

var tabNames = [tabCount]string{"plan", "tasks", "contract", "diff", "logs", "manual"}

// tabFile is the lifecycle file behind a tab ("" for the ones that are not files).
var tabFile = [tabCount]string{"plan.md", "tasks.md", "contract.md", "", "", ""}

func (t Tab) String() string { return tabNames[t] }

// page is what one tab last loaded.
type page struct {
	loaded  bool
	text    string
	err     string                // the load failed
	missing bool                  // the thing does not exist
	manual  api.TestsRenderResult // the manual tab's steps
}

// detail is the per-ticket view: tabs over the ticket's files, diff and logs.
type detail struct {
	ticket string
	tab    Tab
	pages  [tabCount]page
	scroll [tabCount]int
	gen    int // distinguishes this opening from an earlier one, for stale timers
	step   int // the selected manual-test step
	// stepLine is the screen line of the selected step, set while laying out the tab.
	stepLine int
}

func (d *detail) page() page { return d.pages[d.tab] }

// switchTo moves to a tab (wrapping), reporting whether it still needs loading.
func (d *detail) switchTo(t Tab) (needsLoad bool) {
	d.tab = Tab((int(t)%int(tabCount) + int(tabCount)) % int(tabCount))
	return !d.pages[d.tab].loaded
}

var checkboxRe = regexp.MustCompile(`(?m)^\s*[-*]\s+\[([ xX])\]`)

// TaskProgress counts the checkboxes of a tasks.md: done and total.
func TaskProgress(md string) (done, total int) {
	for _, m := range checkboxRe.FindAllStringSubmatch(md, -1) {
		total++
		if m[1] != " " {
			done++
		}
	}
	return done, total
}

var proposalHeadRe = regexp.MustCompile(`(?m)^### P-\S+ · from \S+ · open$`)

// label is the tab's title, with a count where it helps.
func (d *detail) label(t Tab) string {
	name := tabNames[t]
	p := d.pages[t]
	switch {
	case t == TabTasks && p.loaded && p.err == "" && !p.missing:
		if done, total := TaskProgress(p.text); total > 0 {
			return fmt.Sprintf("%s %d/%d", name, done, total)
		}
	case t == TabManual && p.loaded && p.err == "" && !p.missing:
		if n := len(p.manual.Steps); n > 0 {
			return fmt.Sprintf("%s %d/%d", name, doneSteps(p.manual), n)
		}
	case t == TabContract && p.loaded && p.err == "" && !p.missing:
		if n := len(proposalHeadRe.FindAllString(p.text, -1)); n > 0 {
			return fmt.Sprintf("%s (%d open)", name, n)
		}
	}
	return name
}

// tabBar draws the tab titles with the current one in brackets.
func (d *detail) tabBar(w int) string {
	parts := make([]string, tabCount)
	for t := Tab(0); t < tabCount; t++ {
		if t == d.tab {
			parts[t] = "[" + d.label(t) + "]"
		} else {
			parts[t] = " " + d.label(t) + " "
		}
	}
	return ansi.Truncate(d.ticket+"  "+strings.Join(parts, ""), w, "…")
}

// body is the current tab's text laid out for w columns: wrapped, and with the task
// progress bar above the checklist.
func (d *detail) body(w int) []string {
	p := d.page()
	if d.tab == TabManual && p.loaded && p.err == "" && !p.missing {
		return d.manualLines(w)
	}
	var text string
	switch {
	case !p.loaded:
		text = "loading…"
	case p.missing:
		text = d.missingText()
	case p.err != "":
		text = "Could not load " + tabNames[d.tab] + ":\n" + p.err
	default:
		text = p.text
		if d.tab == TabTasks {
			if done, total := TaskProgress(text); total > 0 {
				text = progressBar(done, total, 20) + "\n\n" + text
			}
		}
		if strings.TrimSpace(text) == "" {
			text = "(empty)"
		}
	}
	return wrapLines(text, w)
}

func (d *detail) missingText() string {
	switch d.tab {
	case TabContract:
		return "This ticket has no contract.md; only tickets with several services have one."
	case TabDiff:
		return "Nothing to show yet: the ticket has no checkouts. Start it first."
	case TabLogs:
		return "No container logs: the ticket has no running containers."
	case TabManual:
		return "No manual-tests.md yet: the agent writes it during the test phase."
	}
	return tabFile[d.tab] + " does not exist yet."
}

func progressBar(done, total, width int) string {
	filled := done * width / total
	return fmt.Sprintf("%s%s  %d/%d done", strings.Repeat("█", filled), strings.Repeat("░", width-filled), done, total)
}

// wrapLines splits text into screen lines no wider than w, wrapping long ones.
func wrapLines(text string, w int) []string {
	if w < 1 {
		w = 1
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		line = strings.ReplaceAll(line, "\r", "")
		line = strings.ReplaceAll(line, "\t", "    ")
		if line == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Hardwrap(line, w, true), "\n")...)
	}
	return out
}

// view draws the tab bar and the scrolled page in a w x h box, clamping the scroll.
func (d *detail) view(w, h int) string {
	bar := d.tabBar(w)
	room := h - 1
	if room < 1 {
		return bar
	}
	lines := d.body(w)
	maxTop := max(len(lines)-room, 0)
	if d.tab == TabManual {
		d.followStep(len(lines), room)
	}
	top := min(max(d.scroll[d.tab], 0), maxTop)
	d.scroll[d.tab] = top
	end := min(top+room, len(lines))
	out := append([]string{bar}, lines[top:end]...)
	return strings.Join(out, "\n")
}

// scrollBy moves the current tab by n lines; view clamps it.
func (d *detail) scrollBy(n int) { d.scroll[d.tab] = max(d.scroll[d.tab]+n, 0) }

func (d *detail) scrollTo(n int) { d.scroll[d.tab] = max(n, 0) }

// bottom scrolls to the end; view clamps it to the last page.
func (d *detail) bottom() { d.scroll[d.tab] = 1 << 30 }

func doneSteps(r api.TestsRenderResult) int {
	n := 0
	for _, s := range r.Steps {
		if s.Done {
			n++
		}
	}
	return n
}

// manualLines lays out the manual tests: a summary, the preamble, then a line per step
// with the selected one expanded to show its text.
func (d *detail) manualLines(w int) []string {
	r := d.page().manual
	d.step = min(max(d.step, 0), max(len(r.Steps)-1, 0))
	var lines []string
	switch {
	case len(r.Steps) == 0:
		lines = append(lines, "No steps yet: the agent writes them as `### <id>: <title>` sections in manual-tests.md.")
	case r.SignedOff:
		lines = append(lines, fmt.Sprintf("SIGNED OFF · %d/%d steps done", doneSteps(r), len(r.Steps)))
	default:
		lines = append(lines, fmt.Sprintf("%d/%d steps done · not signed off (S to sign off)", doneSteps(r), len(r.Steps)))
	}
	if strings.TrimSpace(r.Preamble) != "" {
		lines = append(lines, "")
		lines = append(lines, wrapLines(r.Preamble, w)...)
	}
	lines = append(lines, "")
	for i, s := range r.Steps {
		box := "[ ]"
		if s.Done {
			box = "[x]"
		}
		cursor := "  "
		if i == d.step {
			cursor = "> "
			d.stepLine = len(lines)
		}
		lines = append(lines, wrapLines(fmt.Sprintf("%s%s %s: %s", cursor, box, s.ID, s.Title), w)...)
		if i == d.step {
			for _, l := range wrapLines(s.Body, max(w-6, 1)) {
				lines = append(lines, "      "+l)
			}
			lines = append(lines, "")
		}
	}
	return lines
}

// followStep scrolls the manual tab so the selected step's line is on screen.
func (d *detail) followStep(total, room int) {
	top := d.scroll[TabManual]
	switch {
	case d.stepLine < top:
		top = d.stepLine
	case d.stepLine >= top+room:
		top = d.stepLine - room + 1
	}
	d.scroll[TabManual] = max(top, 0)
}

// CodeBlocks returns the text inside the fenced code blocks of a step's body, one string
// per block, without the fences.
func CodeBlocks(body string) []string {
	var blocks []string
	var cur []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if in {
				blocks = append(blocks, strings.Join(cur, "\n"))
				cur = nil
			}
			in = !in
			continue
		}
		if in {
			cur = append(cur, line)
		}
	}
	if text := strings.Join(cur, "\n"); in && strings.TrimSpace(text) != "" { // a block the author never closed is still a block
		blocks = append(blocks, text)
	}
	return blocks
}
