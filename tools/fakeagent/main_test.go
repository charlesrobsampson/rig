package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/testkit"
)

func runIn(t *testing.T, dir, script string, extra ...string) (code int, stdout, stderr string) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "script.txt")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	var out, errOut bytes.Buffer
	code = run(context.Background(), append([]string{"-script", scriptPath}, extra...), strings.NewReader(""), &out, &errOut, func(k string) string {
		return map[string]string{"RIG_SERVICE": "api"}[k]
	})
	return code, out.String(), errOut.String()
}

func TestTheBinaryRunsAScriptInTheCurrentDirectory(t *testing.T) {
	repo := testkit.NewRepo(t)
	code, stdout, stderr := runIn(t, repo.Dir, "write hello.txt \"hi\\n\"\ncommit \"say hello\"\nsay finished\n")
	if code != 0 || !strings.Contains(stdout, "finished") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := repo.Git("log", "-1", "--format=%s"); got != "say hello" {
		t.Errorf("last commit = %q", got)
	}
}

func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name   string
		script string
		code   int
		text   string
	}{
		{"a script that ends", "say done\n", 0, ""},
		{"exit 7", "exit 7\n", 7, ""},
		{"a bad script", "write a \"x\"\nbogus\n", 2, "line 2"},
		{"a failure while running", "commit \"nothing here is a repo\"\n", 1, "line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runIn(t, dir, tt.script)
			if code != tt.code || !strings.Contains(stderr, tt.text) {
				t.Errorf("code %d (want %d), stderr %q", code, tt.code, stderr)
			}
		})
	}
}

func TestStateReporting(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "state")
	marker := filepath.Join(t.TempDir(), "cmd-ran-with")
	code, stdout, stderr := runIn(t, dir, "state working\nstate idle\n", "-state-file", file, "-state-cmd", "echo >> "+marker)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if got, _ := os.ReadFile(file); string(got) != "idle" {
		t.Errorf("state file = %q, want the latest state", got)
	}
	if got, _ := os.ReadFile(marker); strings.TrimSpace(string(got)) != "working\nidle" && !strings.Contains(string(got), "idle") {
		t.Errorf("state command output = %q", got)
	}
	if !strings.Contains(stdout, "state=working") || !strings.Contains(stdout, "state=idle") {
		t.Errorf("stdout = %q, want the states on screen", stdout)
	}
}

func TestUsageErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, strings.NewReader(""), &out, &errOut, func(string) string { return "" }); code != 2 || !strings.Contains(errOut.String(), "-script") {
		t.Errorf("no script: code %d, stderr %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"-script", "/no/such/file"}, strings.NewReader(""), &out, &errOut, func(string) string { return "" }); code != 2 {
		t.Errorf("missing file: code %d", code)
	}
	if code := run(context.Background(), []string{"-bogus"}, strings.NewReader(""), &out, &errOut, func(string) string { return "" }); code != 2 {
		t.Errorf("bad flag: code %d", code)
	}
}
