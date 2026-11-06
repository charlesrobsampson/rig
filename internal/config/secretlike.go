package config

import (
	"math"
	"regexp"
	"strings"
	"unicode"
)

var secretPatterns = regexp.MustCompile(`^(` + strings.Join([]string{
	`sk-ant-[A-Za-z0-9_-]{10,}`,
	`sk-[A-Za-z0-9]{20,}`,
	`gh[pousr]_[A-Za-z0-9]{20,}`,
	`github_pat_[A-Za-z0-9_]{20,}`,
	`xox[abprs]-[A-Za-z0-9-]{10,}`,
	`(AKIA|ASIA)[0-9A-Z]{16}`,
	`AIza[0-9A-Za-z_-]{30,}`,
	`-----BEGIN [A-Z ]*PRIVATE KEY-----.*`,
	`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\..*`,
}, "|") + `)$`)

var urlCredentials = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)

// looksLikeSecret flags values that are probably credentials, so they get listed
// under `secrets:` (resolved from the keychain) instead of being written to disk.
// The entropy floor sits above hex digests such as git SHAs (about 4.0 bits).
func looksLikeSecret(v string) bool {
	if secretPatterns.MatchString(v) || urlCredentials.MatchString(v) {
		return true
	}
	return len(v) >= 32 && !strings.ContainsFunc(v, unicode.IsSpace) && entropy(v) >= 4.2
}

func entropy(s string) float64 {
	counts := map[rune]float64{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, c := range counts {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}
