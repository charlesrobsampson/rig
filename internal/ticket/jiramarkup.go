package ticket

import (
	"regexp"
	"strings"
)

var (
	jiraHeading   = regexp.MustCompile(`^h([1-6])\.\s+(.*)$`)
	jiraBullet    = regexp.MustCompile(`^(\*+)\s+(.*)$`)
	jiraNumbered  = regexp.MustCompile(`^(#+)\s+(.*)$`)
	jiraLink      = regexp.MustCompile(`\[([^\]|]+)\|([^\]]+)\]`)
	jiraBareLink  = regexp.MustCompile(`\[(https?://[^\]\s|]+)\]`)
	jiraMono      = regexp.MustCompile(`\{\{([^}]+)\}\}`)
	jiraBold      = regexp.MustCompile(`(^|[\s(])\*([^*\s][^*]*[^*\s]|[^*\s])\*($|[\s).,;:!?])`)
	jiraStrike    = regexp.MustCompile(`(^|[\s(])-([^-\s][^-]*[^-\s]|[^-\s])-($|[\s).,;:!?])`)
	jiraCodeOpen  = regexp.MustCompile(`^\{code(?::([A-Za-z0-9+#_-]+))?[^}]*\}`)
	jiraTableLine = regexp.MustCompile(`^\|`)
	jiraColor     = regexp.MustCompile(`\{color:[^}]*\}|\{color\}`)
	jiraQuoteOpen = regexp.MustCompile(`^\{quote\}$`)
)

// JiraMarkupToMarkdown converts the part of Jira's wiki markup people actually use in
// descriptions into Markdown: headings, bold, monospace, links, bullet and numbered lists
// (nested), code and noformat blocks, quotes and simple tables. Anything it does not know
// is left as written, so nothing is lost, only left unconverted.
func JiraMarkupToMarkdown(in string) string {
	in = strings.ReplaceAll(strings.ReplaceAll(in, "\r\n", "\n"), "\r", "\n")
	var out []string
	lines := strings.Split(in, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// {code} and {noformat} blocks pass through untouched
		if m := jiraCodeOpen.FindStringSubmatch(trimmed); m != nil || trimmed == "{noformat}" {
			lang := ""
			if m != nil {
				lang = m[1]
			}
			rest := ""
			if m != nil {
				rest = strings.TrimSpace(strings.TrimPrefix(trimmed, m[0]))
			}
			out = append(out, "```"+lang)
			if rest != "" { // {code}one line{code}
				if end := strings.Index(rest, "{code}"); end >= 0 {
					out = append(out, rest[:end], "```")
					continue
				}
				out = append(out, rest)
			}
			for i++; i < len(lines); i++ {
				l := lines[i]
				if t := strings.TrimSpace(l); t == "{code}" || t == "{noformat}" {
					break
				}
				if strings.HasSuffix(strings.TrimSpace(l), "{code}") {
					out = append(out, strings.TrimSuffix(strings.TrimSpace(l), "{code}"))
					break
				}
				out = append(out, l)
			}
			out = append(out, "```")
			continue
		}

		switch {
		case jiraQuoteOpen.MatchString(trimmed):
			for i++; i < len(lines) && !jiraQuoteOpen.MatchString(strings.TrimSpace(lines[i])); i++ {
				out = append(out, "> "+jiraInline(lines[i]))
			}
		case strings.HasPrefix(trimmed, "bq. "):
			out = append(out, "> "+jiraInline(strings.TrimPrefix(trimmed, "bq. ")))
		case jiraHeading.MatchString(trimmed):
			m := jiraHeading.FindStringSubmatch(trimmed)
			out = append(out, strings.Repeat("#", int(m[1][0]-'0'))+" "+jiraInline(m[2]))
		case jiraBullet.MatchString(trimmed):
			m := jiraBullet.FindStringSubmatch(trimmed)
			out = append(out, strings.Repeat("  ", len(m[1])-1)+"- "+jiraInline(m[2]))
		case jiraNumbered.MatchString(trimmed):
			m := jiraNumbered.FindStringSubmatch(trimmed)
			out = append(out, strings.Repeat("   ", len(m[1])-1)+"1. "+jiraInline(m[2]))
		case jiraTableLine.MatchString(trimmed):
			start := i
			var rows []string
			for ; i < len(lines) && jiraTableLine.MatchString(strings.TrimSpace(lines[i])); i++ {
				rows = append(rows, strings.TrimSpace(lines[i]))
			}
			i--
			out = append(out, jiraTable(rows, start)...)
		case trimmed == "----":
			out = append(out, "---")
		default:
			out = append(out, jiraInline(line))
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// jiraTable turns wiki table rows (|| header || and | cell |) into a Markdown table.
func jiraTable(rows []string, _ int) []string {
	var out []string
	for n, row := range rows {
		header := strings.HasPrefix(row, "||")
		sep := "|"
		if header {
			sep = "||"
		}
		cells := strings.Split(strings.Trim(row, "|"), sep)
		for i, c := range cells {
			cells[i] = jiraInline(strings.TrimSpace(c))
		}
		out = append(out, "| "+strings.Join(cells, " | ")+" |")
		if n == 0 {
			bars := make([]string, len(cells))
			for i := range bars {
				bars[i] = "---"
			}
			if header {
				out = append(out, "| "+strings.Join(bars, " | ")+" |")
			} else { // Markdown needs a header row; make an empty one
				empty := make([]string, len(cells))
				out = []string{"| " + strings.Join(empty, " | ") + " |", "| " + strings.Join(bars, " | ") + " |", out[0]}
			}
		}
	}
	return out
}

// jiraInline converts the formatting inside a line.
func jiraInline(s string) string {
	s = jiraColor.ReplaceAllString(s, "")
	s = jiraMono.ReplaceAllString(s, "`$1`")
	s = jiraLink.ReplaceAllString(s, "[$1]($2)")
	s = jiraBareLink.ReplaceAllString(s, "<$1>")
	s = jiraBold.ReplaceAllString(s, "$1**$2**$3")
	s = jiraStrike.ReplaceAllString(s, "$1~~$2~~$3")
	s = strings.ReplaceAll(s, "\\\\", "  \n")
	return s
}
