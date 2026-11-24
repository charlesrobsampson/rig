package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/charlesrobsampson/rig/internal/domain"
)

var (
	// ErrGHMissing: the GitHub CLI is not installed.
	ErrGHMissing = errors.New("GitHub CLI (gh) is not installed")
	// ErrGHAuth: gh is installed but not logged in.
	ErrGHAuth = errors.New("GitHub CLI (gh) is not logged in")
	// ErrPR: opening or finding the pull request failed.
	ErrPR = errors.New("pull request failed")
)

// GHResult is what one gh invocation produced. A non-zero ExitCode is not a Go error.
type GHResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// GH runs the GitHub CLI in dir with stdin, adding env to rig's own environment. Run
// returns ErrGHMissing when gh is not installed and an error only when it could not be
// run at all.
type GH interface {
	Run(ctx context.Context, dir, stdin string, env []string, args ...string) (GHResult, error)
}

// NewGH returns a GH that runs the `gh` binary (or another path) with the user's own
// login: nothing of gh's credentials is read or copied by rig.
func NewGH(binary string) GH {
	if binary == "" {
		binary = "gh"
	}
	return execGH{binary}
}

type execGH struct{ binary string }

func (g execGH) Run(ctx context.Context, dir, stdin string, env []string, args ...string) (GHResult, error) {
	path, err := exec.LookPath(g.binary)
	if err != nil {
		return GHResult{}, ErrGHMissing
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = append(append(baseEnv(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	res := GHResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return GHResult{}, ctx.Err()
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return GHResult{}, fmt.Errorf("run gh: %w", err)
	}
	return res, nil
}

// PROptions describe one pull request: the branch is pushed first, then the PR opened
// against Base with the given title and body.
type PROptions struct {
	PushOptions
	// GitHubUser opens the pull request as this gh account instead of the active one.
	GitHubUser domain.GitHubLogin
	Title      string
	Body       string
	Draft      bool
}

// PushPR pushes the branch and opens a pull request for it, or reuses the one already
// open for that branch. gh, the account and its access to the repository are checked
// before anything is pushed, so a failure there does not leave a pushed branch behind
// for nothing.
func PushPR(ctx context.Context, gh GH, o PROptions) (PushResult, error) {
	res := PushResult{Mode: domain.PushPR}
	if o.Base == "" {
		return res, errors.New("pull request mode needs a base branch")
	}
	if _, err := branchTip(ctx, o.PushOptions); err != nil {
		return res, err
	}
	env, err := checkGH(ctx, gh, o.Repo, o.GitHubUser)
	if err != nil {
		return res, err
	}
	existing, err := openPR(ctx, gh, o, env)
	if err != nil {
		return res, err
	}

	pushed, err := Push(ctx, domain.PushBranch, o.PushOptions)
	res.Remote, res.URL, res.Ref, res.Sha, res.Pushed = pushed.Remote, pushed.URL, pushed.Ref, pushed.Sha, pushed.Pushed
	if err != nil {
		return res, err
	}
	if existing != "" {
		res.PRURL, res.Note = existing, "a pull request for this branch was already open; the new commits are on it"
		return res, nil
	}

	args := []string{"pr", "create", "--head", o.Branch, "--base", o.Base, "--title", o.Title, "--body-file", "-"}
	if o.Draft {
		args = append(args, "--draft")
	}
	out, err := gh.Run(ctx, o.Repo, o.Body, env, args...)
	if err != nil {
		return res, err
	}
	if out.ExitCode != 0 {
		return res, prFailed("pr create", out)
	}
	url := lastURL(out.Stdout)
	if url == "" {
		return res, fmt.Errorf("%w: gh pr create printed no URL: %s", ErrPR, firstLine(out.Stdout, out.Stderr))
	}
	res.PRURL = url
	return res, nil
}

// checkGH makes sure gh can be used, and returns the environment its pull-request calls
// need: the named account's token, given to those calls only, so the user's active
// account is never switched. The token is never put in an error.
func checkGH(ctx context.Context, gh GH, repo string, user domain.GitHubLogin) ([]string, error) {
	if _, err := gh.Run(ctx, repo, "", nil, "--version"); err != nil {
		if errors.Is(err, ErrGHMissing) {
			return nil, fmt.Errorf("%w; install it from https://cli.github.com and run `gh auth login`", ErrGHMissing)
		}
		return nil, err
	}
	if user == "" {
		out, err := gh.Run(ctx, repo, "", nil, "auth", "status")
		if err != nil {
			return nil, err
		}
		if out.ExitCode != 0 {
			return nil, fmt.Errorf("%w; run `gh auth login`", ErrGHAuth)
		}
		return nil, nil
	}
	out, err := gh.Run(ctx, repo, "", nil, "auth", "token", "--user", string(user))
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(out.Stdout)
	if out.ExitCode != 0 || token == "" {
		return nil, fmt.Errorf("%w as %s; run `gh auth login` and log in as %s (policy.push.github_user)", ErrGHAuth, user, user)
	}
	return []string{"GH_TOKEN=" + token}, nil
}

// prFailed reports a failed gh call in gh's own words, with what to do when the account
// in use cannot see the repository.
func prFailed(call string, out GHResult) error {
	msg := firstLine(out.Stderr, out.Stdout)
	if strings.Contains(msg, "Could not resolve to a Repository") {
		msg += "; the gh account in use cannot see this repository: set policy.push.github_user to one that can (`gh auth status` lists them)"
	}
	return fmt.Errorf("%w: gh %s: %s", ErrPR, call, msg)
}

// openPR returns the URL of the open pull request from the branch into the base, or "".
func openPR(ctx context.Context, gh GH, o PROptions, env []string) (string, error) {
	out, err := gh.Run(ctx, o.Repo, "", env, "pr", "list", "--head", o.Branch, "--base", o.Base, "--state", "open", "--json", "url", "--limit", "1")
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		return "", prFailed("pr list", out)
	}
	var prs []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.Stdout)), &prs); err != nil {
		return "", fmt.Errorf("%w: cannot read gh pr list output %q", ErrPR, firstLine(out.Stdout))
	}
	if len(prs) == 0 {
		return "", nil
	}
	return prs[0].URL, nil
}

func lastURL(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return l
		}
	}
	return ""
}

func firstLine(parts ...string) string {
	for _, p := range parts {
		if line, _, _ := strings.Cut(strings.TrimSpace(p), "\n"); line != "" {
			return line
		}
	}
	return "no output"
}

// PRContent is what a pull request description is built from.
type PRContent struct {
	TicketID string
	Title    string // the ticket's title
	Service  string
	Multi    bool   // the ticket has several services, each with its own PR
	Source   string // "jira PROJ-412", empty for a manual ticket
	Plan     string // plan.md
	Files    []FileChange
}

// maxPlanChars keeps the description far below GitHub's 65,536-character limit.
const maxPlanChars = 20000

// PRTitle is the ticket's title, with the service when there are several PRs.
func PRTitle(c PRContent) string {
	if c.Multi && c.Service != "" {
		return fmt.Sprintf("%s (%s)", c.Title, c.Service)
	}
	return c.Title
}

// PRBody builds the description: where the ticket came from, its plan, and the files
// this service changed.
func PRBody(c PRContent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ticket `%s`", c.TicketID)
	if c.Source != "" {
		fmt.Fprintf(&b, " (%s)", c.Source)
	}
	if c.Multi && c.Service != "" {
		fmt.Fprintf(&b, ", service `%s`", c.Service)
	}
	b.WriteString("\n")

	if plan := strings.TrimSpace(stripTitle(c.Plan)); plan != "" {
		b.WriteString("\n## Plan\n\n")
		if r := []rune(plan); len(r) > maxPlanChars {
			plan = string(r[:maxPlanChars]) + "\n\n_(plan truncated)_"
		}
		b.WriteString(plan + "\n")
	}

	if len(c.Files) > 0 {
		b.WriteString("\n## Changes\n\n| File | + | - |\n|---|---:|---:|\n")
		added, deleted := 0, 0
		for _, f := range c.Files {
			fmt.Fprintf(&b, "| `%s` | %d | %d |\n", f.Path, f.Added, f.Deleted)
			added, deleted = added+f.Added, deleted+f.Deleted
		}
		noun := "files"
		if len(c.Files) == 1 {
			noun = "file"
		}
		fmt.Fprintf(&b, "\n%d %s changed, +%d -%d\n", len(c.Files), noun, added, deleted)
	}
	b.WriteString("\n---\nOpened by [rig](https://github.com/charlesrobsampson/rig).\n")
	return b.String()
}

// stripTitle drops a leading "# Heading" line, which just repeats the PR title.
func stripTitle(plan string) string {
	plan = strings.TrimSpace(plan)
	if strings.HasPrefix(plan, "# ") {
		_, rest, _ := strings.Cut(plan, "\n")
		return rest
	}
	return plan
}
