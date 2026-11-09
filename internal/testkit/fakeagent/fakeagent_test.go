package fakeagent_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/testkit/fakeagent"
)

var ctx = context.Background()

type env struct {
	fakeagent.Env
	out    *bytes.Buffer
	states []string
}

func newEnv(t *testing.T, dir string) *env {
	t.Helper()
	e := &env{out: &bytes.Buffer{}}
	e.Env = fakeagent.Env{
		Dir:     dir,
		Service: "api",
		Stdout:  e.out,
		Stdin:   strings.NewReader(""),
		ReportState: func(s string) error {
			e.states = append(e.states, s)
			return nil
		},
		InputIdle: 50 * time.Millisecond,
	}
	return e
}

func run(t *testing.T, e *env, script string) error {
	t.Helper()
	cmds, err := fakeagent.Parse(script)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return fakeagent.Run(ctx, cmds, e.Env)
}

func TestParse(t *testing.T) {
	cmds, err := fakeagent.Parse(`
# a comment
write notes.txt "line one\nline \"two\""

   state working
commit "first change"
exit 3
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		line int
		name string
		args []string
	}{
		{3, "write", []string{"notes.txt", "line one\nline \"two\""}},
		{5, "state", []string{"working"}},
		{6, "commit", []string{"first change"}},
		{7, "exit", []string{"3"}},
	}
	if len(cmds) != len(want) {
		t.Fatalf("parsed %d commands, want %d: %+v", len(cmds), len(want), cmds)
	}
	for i, w := range want {
		got := cmds[i]
		if got.Line != w.line || got.Name != w.name || strings.Join(got.Args, "|") != strings.Join(w.args, "|") {
			t.Errorf("command %d = %+v, want %+v", i, got, w)
		}
	}
}

func TestParseRejectsBadScriptsWithTheLineNumber(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   []string
	}{
		{"an unknown command", "write a \"x\"\nlaunch-missiles now\n", []string{"line 2", "launch-missiles"}},
		{"write without content", "write onlypath\n", []string{"line 1", "write"}},
		{"commit without a message", "say hi\ncommit\n", []string{"line 2", "commit"}},
		{"an unterminated quote", "\n\nwrite a \"oops\n", []string{"line 3", "quote"}},
		{"an invalid state", "state napping\n", []string{"line 1", "napping"}},
		{"a bad exit code", "exit lots\n", []string{"line 1", "exit"}},
		{"too many arguments", "state working extra\n", []string{"line 1", "state"}},
		{"a bad sleep", "sleep forever\n", []string{"line 1", "sleep"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fakeagent.Parse(tt.script)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestABadScriptDoesNothingBecauseItIsValidatedFirst(t *testing.T) {
	dir := t.TempDir()
	_, err := fakeagent.Parse("write first.txt \"x\"\nbogus\n")
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "first.txt")); statErr == nil {
		t.Error("side effects happened before the script was validated")
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)
	abs := filepath.Join(t.TempDir(), "elsewhere.txt")
	script := "write plan.md \"# Plan\\n\\nstep one\\n\"\nwrite deep/nested/file.txt \"nested\"\nwrite plan.md \"replaced\"\nwrite " + abs + " \"absolute\"\n"
	if err := run(t, e, script); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "plan.md"):              "replaced",
		filepath.Join(dir, "deep/nested/file.txt"): "nested",
		abs: "absolute",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestWriteRefusesToEscapeTheWorkingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(dir, 0o755)
	e := newEnv(t, dir)
	err := run(t, e, "write ../outside.txt \"x\"\n")
	if err == nil || !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "outside") {
		t.Errorf("error = %v, want a refusal naming the line", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "outside.txt")); statErr == nil {
		t.Error("the file was written outside the working directory")
	}
}

func TestCommit(t *testing.T) {
	repo := testkit.NewRepo(t)
	e := newEnv(t, repo.Dir)
	if err := run(t, e, "write feature.txt \"new feature\\n\"\ncommit \"add the feature\"\nwrite more.txt \"x\"\ncommit \"second change\"\n"); err != nil {
		t.Fatal(err)
	}
	log := repo.Git("log", "--format=%s|%an", "-3")
	if lines := strings.Split(log, "\n"); lines[0] != "second change|fakeagent" || lines[1] != "add the feature|fakeagent" {
		t.Errorf("log:\n%s", log)
	}
	if repo.Git("status", "--porcelain") != "" {
		t.Error("the commit left changes behind")
	}

	err := run(t, e, "commit \"nothing changed\"\n")
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("committing nothing = %v, want an error naming the line", err)
	}
}

func TestAppendProposalWritesTheContractFormat(t *testing.T) {
	ticketDir := t.TempDir()
	work := filepath.Join(ticketDir, "api")
	os.MkdirAll(work, 0o755)
	contract := filepath.Join(ticketDir, "contract.md")
	original := "# Contract: demo\n\n## Agreed\nPOST /widgets\n\n## Proposed changes\n\n## Changelog\n- 2026-11-05 earlier change\n"
	os.WriteFile(contract, []byte(original), 0o644)

	e := newEnv(t, work)
	if err := run(t, e, `append-proposal "add locale to the request" "the frontend needs localized names"`+"\n"+`append-proposal "second idea"`+"\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(contract)
	text := string(got)

	header := regexp.MustCompile(`(?m)^### (P-[a-z][a-z0-9-]*-\d+) · from ([a-z][a-z0-9-]*) · (open|accepted|rejected)$`)
	ids := header.FindAllStringSubmatch(text, -1)
	if len(ids) != 2 || ids[0][1] != "P-api-1" || ids[1][1] != "P-api-2" || ids[0][2] != "api" || ids[0][3] != "open" {
		t.Fatalf("proposal headers = %v in:\n%s", ids, text)
	}
	for _, want := range []string{"**What:** add locale to the request", "**Why:** the frontend needs localized names", "**What:** second idea"} {
		if !strings.Contains(text, want) {
			t.Errorf("contract lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "## Agreed\nPOST /widgets") || !strings.Contains(text, "- 2026-11-05 earlier change") {
		t.Errorf("existing content was altered:\n%s", text)
	}
	if strings.Index(text, "P-api-2") > strings.Index(text, "## Changelog") {
		t.Errorf("proposals must go inside Proposed changes, before the Changelog:\n%s", text)
	}

	t.Run("each service numbers its own proposals", func(t *testing.T) {
		other := filepath.Join(ticketDir, "web")
		os.MkdirAll(other, 0o755)
		e2 := newEnv(t, other)
		e2.Service = "web"
		if err := run(t, e2, `append-proposal "web idea"`+"\n"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(contract)
		if !strings.Contains(string(got), "P-web-1") {
			t.Errorf("web's first proposal should be P-web-1:\n%s", got)
		}
	})
}

func TestAppendProposalAddsTheSectionWhenMissing(t *testing.T) {
	ticketDir := t.TempDir()
	work := filepath.Join(ticketDir, "api")
	os.MkdirAll(work, 0o755)
	os.WriteFile(filepath.Join(ticketDir, "contract.md"), []byte("# Contract: demo\n\n## Agreed\nstuff\n"), 0o644)
	if err := run(t, newEnv(t, work), `append-proposal "idea"`+"\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(ticketDir, "contract.md"))
	if !strings.Contains(string(got), "## Proposed changes") || !strings.Contains(string(got), "P-api-1") {
		t.Errorf("contract:\n%s", got)
	}

	if err := run(t, newEnv(t, t.TempDir()), `append-proposal "idea"`+"\n"); err == nil || !strings.Contains(err.Error(), "contract.md") {
		t.Errorf("no contract.md: error = %v, want one saying so", err)
	}
}

func TestStateReportsThroughTheMechanismAndOnTheScreen(t *testing.T) {
	e := newEnv(t, t.TempDir())
	if err := run(t, e, "state working\nstate blocked\nstate idle\n"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.states, ","); got != "working,blocked,idle" {
		t.Errorf("reported states = %s", got)
	}
	for _, want := range []string{"state=working", "state=blocked", "state=idle"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("screen output lacks %q (a screen heuristic should be able to see the state):\n%s", want, e.out.String())
		}
	}

	failing := newEnv(t, t.TempDir())
	failing.ReportState = func(string) error { return errors.New("daemon unreachable") }
	if err := run(t, failing, "state idle\n"); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("a failing reporter = %v, want an error naming the line", err)
	}
}

func TestWaitInputRecordsWhatIsTypedExactly(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)
	pr, pw := io.Pipe()
	e.Stdin = pr
	record := filepath.Join(dir, "received.txt")

	done := make(chan error, 1)
	go func() { done <- run(t, e, "wait-input "+record+"\nwait-input "+record+"\n") }()

	// A multi-line message arrives as a burst of lines, like text typed into a terminal.
	io.WriteString(pw, "first line \"quoted\" $HOME\nsecond line\n")
	time.Sleep(300 * time.Millisecond) // quiet: the first wait-input is done
	io.WriteString(pw, "a later message\n")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(record)
	want := "first line \"quoted\" $HOME\nsecond line\n" + "a later message\n"
	if string(got) != want {
		t.Errorf("recorded %q, want %q", got, want)
	}
}

func TestWaitInputFailsWhenInputEnds(t *testing.T) {
	e := newEnv(t, t.TempDir())
	e.Stdin = strings.NewReader("")
	err := run(t, e, "wait-input in.txt\n")
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error = %v, want one saying input ended", err)
	}
}

func TestExitStopsTheScript(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)
	err := run(t, e, "write before.txt \"x\"\nexit 7\nwrite after.txt \"y\"\n")
	var exit *fakeagent.ExitError
	if !errors.As(err, &exit) || exit.Code != 7 {
		t.Fatalf("error = %v, want an exit with code 7", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "before.txt")); statErr != nil {
		t.Error("commands before exit did not run")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "after.txt")); statErr == nil {
		t.Error("a command after exit ran")
	}
	if err := run(t, newEnv(t, dir), "exit 0\n"); !errors.As(err, &exit) || exit.Code != 0 {
		t.Errorf("exit 0 = %v", err)
	}
	if err := run(t, newEnv(t, dir), "say done\n"); err != nil {
		t.Errorf("a script that just ends returns %v, want nil", err)
	}
}

func TestSayAndSleepRunInOrder(t *testing.T) {
	e := newEnv(t, t.TempDir())
	if err := run(t, e, "say one\nsleep 10ms\nsay two\nsay \"three words\"\n"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(e.out.String()), "\n")
	if got := strings.Join(lines, "|"); got != "one|two|three words" {
		t.Errorf("output = %q", got)
	}
}

func TestCancellationInterruptsWaiting(t *testing.T) {
	e := newEnv(t, t.TempDir())
	pr, _ := io.Pipe() // never written
	e.Stdin = pr
	cmds, _ := fakeagent.Parse("wait-input in.txt\n")
	cancelled, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- fakeagent.Run(cancelled, cmds, e.Env) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait-input ignored cancellation")
	}

	slow, _ := fakeagent.Parse("sleep 1h\n")
	cancelled2, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if err := fakeagent.Run(cancelled2, slow, e.Env); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("sleep = %v, want the deadline", err)
	}
}
