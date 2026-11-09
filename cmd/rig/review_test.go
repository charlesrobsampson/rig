package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func ticketWithWork(t *testing.T, d *deps, repos ...*testkit.Repo) string {
	t.Helper()
	newTicket(t, *d, "demo", repos...)
	home := os.Getenv("RIG_HOME")
	if _, stderr, code := rig(t, *d, "start", "demo", "--sandbox", "host"); code != 0 {
		t.Fatalf("start: %q", stderr)
	}
	for i := range repos {
		dir := filepath.Join(home, "tickets", "demo", "svc"+string(rune('1'+i)))
		os.WriteFile(filepath.Join(dir, "work.txt"), []byte("line one\nline two\n"), 0o600)
	}
	return home
}

func TestReviewStatListsFilesAndCounts(t *testing.T) {
	_, d, repo := newEnv(t)
	ticketWithWork(t, &d, repo, testkit.NewRepo(t))
	stdout, stderr, code := rig(t, d, "review", "demo", "--stat")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"== svc1 (rig/demo/svc1 vs main) ==", "== svc2 (rig/demo/svc2 vs main) ==", "work.txt", "+2 -0 (new, untracked)", "1 file changed, +2 -0"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "line one") {
		t.Error("--stat must not print the patch")
	}
}

func TestReviewFallsBackToAPlainDiffWithANoticeWhenDeltaIsMissing(t *testing.T) {
	_, d, repo := newEnv(t)
	ticketWithWork(t, &d, repo, testkit.NewRepo(t))
	d.delta = func(string, io.Writer) (bool, error) { return false, nil }
	stdout, stderr, code := rig(t, d, "review", "demo")
	if code != 0 || strings.Count(stderr, "delta is not installed") != 1 {
		t.Errorf("code %d, stderr %q (want the notice exactly once, not per service)", code, stderr)
	}
	if !strings.Contains(stdout, "+line one") || strings.Count(stdout, "diff --git") != 2 {
		t.Errorf("stdout:\n%s", stdout)
	}
}

func TestReviewUsesDeltaWhenItIsInstalled(t *testing.T) {
	_, d, repo := newEnv(t)
	ticketWithWork(t, &d, repo)
	var piped []string
	d.delta = func(patch string, out io.Writer) (bool, error) {
		piped = append(piped, patch)
		io.WriteString(out, "[delta output]\n")
		return true, nil
	}
	stdout, stderr, code := rig(t, d, "review", "demo")
	if code != 0 || stderr != "" || len(piped) != 1 || !strings.Contains(piped[0], "+line two") {
		t.Fatalf("code %d, stderr %q, piped %v", code, stderr, piped)
	}
	if !strings.Contains(stdout, "[delta output]") || strings.Contains(stdout, "diff --git") {
		t.Errorf("the plain patch must not be printed as well:\n%s", stdout)
	}

	piped = nil
	stdout, _, _ = rig(t, d, "review", "demo", "--plain")
	if len(piped) != 0 || !strings.Contains(stdout, "+line one") {
		t.Errorf("--plain: piped %v, stdout:\n%s", piped, stdout)
	}
}

func TestReviewJSONAndErrors(t *testing.T) {
	_, d, repo := newEnv(t)
	ticketWithWork(t, &d, repo)
	stdout, _, code := rig(t, d, "review", "demo", "--json")
	var res api.ReviewDiffResult
	if err := json.Unmarshal([]byte(stdout), &res); code != 0 || err != nil || len(res.Services) != 1 || len(res.Services[0].Files) != 1 || !strings.Contains(res.Services[0].Patch, "+line one") {
		t.Errorf("code %d, %v, %q", code, err, stdout)
	}
	for _, tt := range []struct {
		args []string
		code int
	}{{[]string{"review", "ghost"}, 5}, {[]string{"review", "demo", "nope"}, 5}, {[]string{"review"}, 2}} {
		if _, _, code := rig(t, d, tt.args...); code != tt.code {
			t.Errorf("%v: code %d, want %d", tt.args, code, tt.code)
		}
	}
}

func TestReviewWithoutChangesAndBeforeStart(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	stdout, _, code := rig(t, d, "review", "demo")
	if code != 0 || !strings.Contains(stdout, "not started") {
		t.Errorf("before start: code %d, %q", code, stdout)
	}
	rig(t, d, "start", "demo")
	stdout, _, _ = rig(t, d, "review", "demo")
	if !strings.Contains(stdout, "no changes") {
		t.Errorf("a clean checkout: %q", stdout)
	}
}

func TestReviewMoveAdvancesTheTicketIntoReview(t *testing.T) {
	_, d, repo := newEnv(t)
	ticketWithWork(t, &d, repo)
	for _, args := range [][]string{{"approve", "demo", "plan_approval"}, {"phase", "demo", "implement"}, {"phase", "demo", "test"}} {
		if _, stderr, code := rig(t, d, args...); code != 0 {
			t.Fatalf("%v: %q", args, stderr)
		}
	}
	_, stderr, code := rig(t, d, "review", "demo", "--stat", "--move")
	if code != 0 || !strings.Contains(stderr, "test -> review") {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
	if listing, _, _ := rig(t, d, "list"); !strings.Contains(listing, "review") {
		t.Errorf("list:\n%s", listing)
	}
	// a second --move is refused: review -> review is not a move
	if _, _, code := rig(t, d, "review", "demo", "--stat", "--move"); code == 0 {
		t.Error("moving a ticket already in review should fail")
	}
}
