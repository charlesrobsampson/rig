package secrets

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvFile reads KEY=VALUE lines from a file the user keeps outside rig. A missing file
// simply has nothing; one that other users can read is refused, since it holds secrets.
type EnvFile struct {
	Path string
}

func (e *EnvFile) Describe() string { return fmt.Sprintf("env file %s", e.Path) }

func (e *EnvFile) Lookup(_ context.Context, name string) (string, bool, error) {
	if e.Path == "" {
		return "", false, nil
	}
	info, err := os.Stat(e.Path)
	switch {
	case os.IsNotExist(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	case info.Mode().Perm()&0o077 != 0:
		return "", false, fmt.Errorf("%s is readable by other users (mode %o); run chmod 600 on it", e.Path, info.Mode().Perm())
	}
	f, err := os.Open(e.Path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	var value string
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if k, v, ok := parseLine(sc.Text()); ok && k == name {
			value, found = v, true // a later line overrides an earlier one, as in a shell
		}
	}
	return value, found, sc.Err()
}

func parseLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, rest, found := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !found || !envName.MatchString(key) {
		return "", "", false
	}
	rest = strings.TrimSpace(rest)
	if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') {
		if end := strings.IndexByte(rest[1:], rest[0]); end >= 0 {
			return key, rest[1 : 1+end], true
		}
	}
	if i := strings.Index(rest, " #"); i >= 0 { // a trailing comment on an unquoted value
		rest = strings.TrimSpace(rest[:i])
	}
	return key, rest, true
}
