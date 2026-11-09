// Package git runs git for rig. Anything an agent can write to (a clone it works in)
// may carry hostile config, hooks, or attributes, so there are two runners: Trusted
// for the user's own repositories and Hardened for agent-writable ones.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

type Runner struct{ hardened bool }

// Trusted is for the user's own repositories: it keeps the user's git configuration
// (identity, credentials, hooks) and only drops what could redirect git elsewhere.
func Trusted() Runner { return Runner{} }

// Hardened is for directories an agent can write. It ignores user and system config,
// never uses hooks, filesystem monitors, filter drivers, external diff or textconv
// programs, pagers, credential helpers, ssh, or any network protocol, so nothing the
// agent planted in the repository can run on the host.
func Hardened() Runner { return Runner{hardened: true} }

type Error struct {
	Args   []string
	Dir    string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	detail := strings.Join(strings.Fields(e.Stderr), " ")
	if detail == "" {
		detail = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), detail)
}

func (e *Error) Unwrap() error { return e.Err }

// Run executes git in dir and returns its standard output without the trailing newline.
func (r Runner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	env := baseEnv()
	var prefix []string
	if r.hardened {
		env = hardenedEnv(env)
		prefix = hardenedConfig()
		prefix = append(prefix, neutralizeFilters(ctx, dir, env, prefix)...)
	}
	args = addDiffSafety(args, r.hardened)
	return execGit(ctx, dir, env, append(prefix, args...))
}

func execGit(ctx context.Context, dir string, env []string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", &Error{Args: gitArgsForDisplay(args), Dir: dir, Stderr: stderr.String(), Err: err}
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

// repoLocation variables would point git at some other repository than dir.
var repoLocation = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_PREFIX", "GIT_NAMESPACE",
}

func baseEnv() []string {
	env := withoutKeys(os.Environ(), repoLocation...)
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_PAGER=cat", "PAGER=cat", "LC_ALL=C")
}

// hardenedEnv drops every git variable (they can inject config, programs, or remotes)
// and pins the ones that make git inert.
func hardenedEnv(env []string) []string {
	var kept []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "GIT_") && key != "SSH_ASKPASS" {
			kept = append(kept, kv)
		}
	}
	return append(kept,
		"GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_PAGER=cat", "PAGER=cat",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0", "GIT_ATTR_NOSYSTEM=1",
	)
}

// hardenedConfig is passed with -c, which outranks the repository's own config.
func hardenedConfig() []string {
	var args []string
	for _, kv := range []string{
		"core.hooksPath=" + os.DevNull,
		"core.fsmonitor=false",
		"core.pager=cat",
		"core.askPass=",
		"core.sshCommand=false",
		"core.attributesFile=" + os.DevNull,
		"core.excludesFile=" + os.DevNull,
		"credential.helper=",
		"protocol.allow=never",
	} {
		args = append(args, "-c", kv)
	}
	return args
}

// neutralizeFilters disables every filter driver the repository's config defines.
// Filter names are chosen by the repository, so they cannot be listed in advance:
// read the names out of its config (which executes nothing) and blank each driver.
func neutralizeFilters(ctx context.Context, dir string, env, prefix []string) []string {
	out, err := execGit(ctx, dir, env, append(slices.Clone(prefix), "config", "--name-only", "--get-regexp", `^filter\.`))
	if err != nil {
		return nil // no filters, or not a repository; the real command will report it
	}
	seen := map[string]bool{}
	var args []string
	for _, key := range strings.Fields(out) {
		name := strings.TrimSuffix(strings.TrimPrefix(key, "filter."), key[strings.LastIndex(key, "."):])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		for _, setting := range []string{"clean=", "smudge=", "process=", "required=false"} {
			args = append(args, "-c", "filter."+name+"."+setting)
		}
	}
	return args
}

// addDiffSafety stops diff-producing commands from running external diff programs and
// textconv drivers, which repository config can name even outside hardened use.
func addDiffSafety(args []string, hardened bool) []string {
	if !hardened {
		return args
	}
	i := subcommandIndex(args)
	if i < 0 {
		return args
	}
	switch args[i] {
	case "diff", "log", "show":
		out := slices.Clone(args[:i+1])
		out = append(out, "--no-ext-diff", "--no-textconv")
		return append(out, args[i+1:]...)
	}
	return args
}

func subcommandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-C" || args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return i
		}
	}
	return -1
}

func gitArgsForDisplay(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" && i+1 < len(args) {
			i++ // hardening noise does not belong in error messages
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func withoutKeys(env []string, keys ...string) []string {
	var out []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(keys, key) {
			out = append(out, kv)
		}
	}
	return out
}

// RunAllowing is Run for commands whose useful answer comes with a non-zero exit, such
// as `git diff --no-index`, which exits 1 when the files differ. Exit codes in allowed
// are not errors; any other failure is.
func (r Runner) RunAllowing(ctx context.Context, dir string, allowed []int, args ...string) (string, error) {
	env := baseEnv()
	var prefix []string
	if r.hardened {
		env = hardenedEnv(env)
		prefix = hardenedConfig()
		prefix = append(prefix, neutralizeFilters(ctx, dir, env, prefix)...)
	}
	args = append(prefix, addDiffSafety(args, r.hardened)...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && slices.Contains(allowed, exitErr.ExitCode()) {
			return strings.TrimRight(stdout.String(), "\r\n"), nil
		}
		return "", &Error{Args: gitArgsForDisplay(args), Dir: dir, Stderr: stderr.String(), Err: err}
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}
