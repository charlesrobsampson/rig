package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
)

func ticketWithRunningService(t *testing.T) (home string, d deps) {
	t.Helper()
	home, d, repo, _, _ := newEnvFull(t)
	repo.Commit("Dockerfile", "FROM scratch\n", "add a Dockerfile")
	newTicket(t, d, "demo", repo)
	yamlPath := filepath.Join(home, "tickets", "demo", "ticket.yaml")
	raw, _ := os.ReadFile(yamlPath)
	os.WriteFile(yamlPath, []byte(strings.Replace(string(raw), "      branch: rig/", "      build: {dockerfile: Dockerfile}\n      internal_port: 8080\n      branch: rig/", 1)), 0o600)
	if _, stderr, code := rig(t, d, "start", "demo"); code != 0 {
		t.Fatalf("start: %q", stderr)
	}
	return home, d
}

func TestTestsPrintsTheRenderedStepsWithLiveAddresses(t *testing.T) {
	home, d := ticketWithRunningService(t)
	os.WriteFile(filepath.Join(home, "tickets", "demo", "manual-tests.md"), []byte(strings.Join([]string{
		"Check {{ticket.id}} by hand.", "",
		"### health: the health page responds", "", "```sh", "curl {{svc1.url}}/healthz", "```", "",
		"### inside: the service is reachable by name", "", "{{svc1.internal_url}}",
	}, "\n")), 0o600)

	stdout, stderr, code := rig(t, d, "tests", "demo")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	jsonOut, _, _ := rig(t, d, "tests", "demo", "--json")
	var res api.TestsRenderResult
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil || len(res.Steps) != 2 {
		t.Fatalf("--json: %v, %q", err, jsonOut)
	}
	port := strings.TrimPrefix(strings.SplitN(strings.SplitN(res.Steps[0].Body, "http://localhost:", 2)[1], "/", 2)[0], "")
	if port == "" || !strings.HasPrefix(port, "41") {
		t.Fatalf("no host port in %q", res.Steps[0].Body)
	}

	// the command prints exactly what the renderer's result says
	var want bytes.Buffer
	writeTests(&want, "demo", res)
	if stdout != want.String() {
		t.Errorf("output differs from the rendered result:\n%s\nwant:\n%s", stdout, want.String())
	}
	for _, line := range []string{"Manual tests for demo", "Check demo by hand.", "[ ] health: the health page responds", "    curl http://localhost:" + port + "/healthz", "[ ] inside: the service is reachable by name", "    http://svc1:8080"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("output lacks %q:\n%s", line, stdout)
		}
	}
	if strings.Contains(stdout, "{{") {
		t.Errorf("a placeholder was left unfilled:\n%s", stdout)
	}
}

func TestTestsShowsWhatAHumanHasTickedOff(t *testing.T) {
	home, d := ticketWithRunningService(t)
	os.WriteFile(filepath.Join(home, "tickets", "demo", "manual-tests.md"), []byte("### a: first\none\n### b: second\ntwo\n"), 0o600)
	statePath := filepath.Join(home, "tickets", "demo", "state.json")
	raw, _ := os.ReadFile(statePath)
	var st map[string]any
	json.Unmarshal(raw, &st)
	st["manual_tests"] = map[string]any{"steps": map[string]any{"a": true}, "signed_off": true}
	out, _ := json.Marshal(st)
	os.WriteFile(statePath, out, 0o600)

	stdout, _, code := rig(t, d, "tests", "demo")
	if code != 0 || !strings.Contains(stdout, "[x] a: first") || !strings.Contains(stdout, "[ ] b: second") || !strings.Contains(stdout, "signed off") {
		t.Errorf("code %d, stdout:\n%s", code, stdout)
	}
}

func TestTestsHintsWhenThereIsNoFileOrNoSteps(t *testing.T) {
	home, d := ticketWithRunningService(t)
	os.Remove(filepath.Join(home, "tickets", "demo", "manual-tests.md"))
	_, stderr, code := rig(t, d, "tests", "demo")
	if code != 5 || !strings.Contains(stderr, "no manual-tests.md yet") || !strings.Contains(stderr, "test phase") {
		t.Errorf("missing file: code %d, stderr %q", code, stderr)
	}

	os.WriteFile(filepath.Join(home, "tickets", "demo", "manual-tests.md"), []byte("# Manual tests\n\nNothing yet.\n"), 0o600)
	stdout, _, code := rig(t, d, "tests", "demo")
	if code != 0 || !strings.Contains(stdout, "no steps yet") || !strings.Contains(stdout, "### <id>: <title>") {
		t.Errorf("no steps: code %d, stdout %q", code, stdout)
	}
}

func TestTestsListsEveryProblemInTheFile(t *testing.T) {
	home, d := ticketWithRunningService(t)
	os.WriteFile(filepath.Join(home, "tickets", "demo", "manual-tests.md"), []byte("### a: x\n{{nope.url}} {{svc1.bogus}}\n"), 0o600)
	_, stderr, code := rig(t, d, "tests", "demo")
	if code != 2 || !strings.Contains(stderr, "cannot be rendered") || !strings.Contains(stderr, "nope") || !strings.Contains(stderr, "bogus") {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
	for _, args := range [][]string{{"tests"}, {"tests", "ghost"}} {
		want := map[bool]int{true: 2, false: 5}[len(args) == 1]
		if _, _, code := rig(t, d, args...); code != want {
			t.Errorf("%v: code %d, want %d", args, code, want)
		}
	}
}
