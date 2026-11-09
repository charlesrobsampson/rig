package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/charlesrobsampson/rig/internal/git"
)

// FakeGH is an in-memory GitHub CLI: it keeps pull requests and answers the few
// commands rig uses (--version, auth status, pr list, pr create) the way gh does, and
// records every call. Missing and Unauthenticated switch the failure modes on.
type FakeGH struct {
	mu              sync.Mutex
	Missing         bool
	Unauthenticated bool
	prs             []FakePR
	calls           [][]string
	next            int
}

var _ git.GH = (*FakeGH)(nil)

// FakePR is a pull request the fake holds.
type FakePR struct {
	URL, Dir, Head, Base, Title, Body string
	Draft, Open                       bool
}

func NewFakeGH() *FakeGH { return &FakeGH{} }

// PRs returns the pull requests created so far.
func (f *FakeGH) PRs() []FakePR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakePR(nil), f.prs...)
}

// Calls returns every invocation's arguments, in order.
func (f *FakeGH) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

// Close marks a pull request closed, so it no longer counts as open.
func (f *FakeGH) Close(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prs {
		if f.prs[i].URL == url {
			f.prs[i].Open = false
		}
	}
}

func (f *FakeGH) Run(_ context.Context, dir, stdin string, args ...string) (git.GHResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.Missing {
		return git.GHResult{}, git.ErrGHMissing
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	has := func(name string) bool {
		for _, a := range args {
			if a == name {
				return true
			}
		}
		return false
	}

	switch {
	case len(args) == 1 && args[0] == "--version":
		return git.GHResult{Stdout: "gh version 2.0.0 (fake)\n"}, nil
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		if f.Unauthenticated {
			return git.GHResult{ExitCode: 1, Stderr: "You are not logged into any GitHub hosts. To log in, run: gh auth login\n"}, nil
		}
		return git.GHResult{Stdout: "github.com\n  ✓ Logged in\n"}, nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
		var found []map[string]string
		for _, pr := range f.prs {
			if pr.Open && pr.Dir == dir && pr.Head == flag("--head") && pr.Base == flag("--base") {
				found = append(found, map[string]string{"url": pr.URL})
			}
		}
		out, _ := json.Marshal(found)
		if found == nil {
			out = []byte("[]")
		}
		return git.GHResult{Stdout: string(out) + "\n"}, nil
	case len(args) >= 2 && args[0] == "pr" && args[1] == "create":
		if flag("--body-file") != "-" || flag("--title") == "" {
			return git.GHResult{ExitCode: 1, Stderr: "fake gh: pr create needs --title and --body-file -\n"}, nil
		}
		f.next++
		pr := FakePR{
			URL: fmt.Sprintf("https://github.com/acme/%s/pull/%d", strings.ReplaceAll(strings.Trim(dir, "/"), "/", "-"), f.next),
			Dir: dir, Head: flag("--head"), Base: flag("--base"), Title: flag("--title"), Body: stdin, Draft: has("--draft"), Open: true,
		}
		f.prs = append(f.prs, pr)
		return git.GHResult{Stdout: "\nCreating pull request for " + pr.Head + " into " + pr.Base + "\n\n" + pr.URL + "\n"}, nil
	}
	return git.GHResult{ExitCode: 1, Stderr: "fake gh: unsupported command " + strings.Join(args, " ") + "\n"}, nil
}
