// Package fakeagent is a scriptable stand-in for an AI coding agent, so tests never
// need a real one. A script is a list of commands (write a file, commit, propose a
// contract change, report a state, wait for typed input, exit) that run in order.
// It runs in-process for tests and, through tools/fakeagent, as a real program inside
// a terminal pane or container.
//
// Script syntax: one command per line; blank lines and lines starting with # are
// ignored; arguments are bare words or "double-quoted" with Go escapes (\n, \", \\).
//
//	write <path> <content>        write a file (relative paths stay inside the working directory)
//	commit <message>              git add -A and commit
//	append-proposal <what> [why]  add a proposal to ../contract.md (docs/formats.md §11)
//	state working|blocked|idle    report a state
//	wait-input <file>             block for typed input, append it to <file>
//	say <text>                    print a line
//	sleep <duration>              wait
//	exit [code]                   stop the script with a status
package fakeagent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Command struct {
	Line int
	Name string
	Args []string
}

// ExitError is what Run returns for an exit command.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("script exited with code %d", e.Code) }

type Env struct {
	// Dir is where relative paths and git commands apply (default: the process's).
	Dir string
	// TicketDir holds contract.md (default: the parent of Dir).
	TicketDir string
	// Service names the agent's service, for numbering proposals.
	Service string
	Stdin   io.Reader
	Stdout  io.Writer
	// ReportState is how a state command tells the world (a hook call, a file, ...).
	// The state is also printed to Stdout, where a screen heuristic can see it.
	ReportState func(state string) error
	// InputIdle is how long wait-input waits for more lines after the first (default 300ms).
	InputIdle time.Duration
}

var states = map[string]bool{"working": true, "blocked": true, "idle": true}

// Parse checks a whole script before anything runs, so a typo on line 40 does not
// leave the effects of lines 1 to 39 behind.
func Parse(script string) ([]Command, error) {
	var cmds []Command
	for i, raw := range strings.Split(script, "\n") {
		line := i + 1
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields, err := tokenize(text)
		if err != nil {
			return nil, fmt.Errorf("script line %d: %w", line, err)
		}
		cmd := Command{Line: line, Name: fields[0], Args: fields[1:]}
		if err := validate(cmd); err != nil {
			return nil, fmt.Errorf("script line %d: %w", line, err)
		}
		cmds = append(cmds, cmd)
	}
	return cmds, nil
}

func tokenize(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		switch {
		case s[i] == ' ' || s[i] == '\t':
			i++
		case s[i] == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, errors.New("unterminated quote")
			}
			unquoted, err := strconv.Unquote(s[i : j+1])
			if err != nil {
				return nil, fmt.Errorf("bad quoted argument %s: %w", s[i:j+1], err)
			}
			out = append(out, unquoted)
			i = j + 1
		default:
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '\t' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out, nil
}

func validate(c Command) error {
	arity := func(min, max int) error {
		if len(c.Args) < min || len(c.Args) > max {
			return fmt.Errorf("%s takes %s", c.Name, plural(min, max))
		}
		return nil
	}
	switch c.Name {
	case "write":
		return arity(2, 2)
	case "commit":
		return arity(1, 1)
	case "append-proposal":
		return arity(1, 2)
	case "wait-input":
		return arity(1, 1)
	case "say":
		return arity(1, 1<<10)
	case "state":
		if err := arity(1, 1); err != nil {
			return err
		}
		if !states[c.Args[0]] {
			return fmt.Errorf("state must be working, blocked, or idle, not %q", c.Args[0])
		}
	case "sleep":
		if err := arity(1, 1); err != nil {
			return err
		}
		if _, err := time.ParseDuration(c.Args[0]); err != nil {
			return fmt.Errorf("sleep needs a duration like 500ms: %w", err)
		}
	case "exit":
		if err := arity(0, 1); err != nil {
			return err
		}
		if len(c.Args) == 1 {
			if _, err := strconv.Atoi(c.Args[0]); err != nil {
				return fmt.Errorf("exit needs a numeric status, not %q", c.Args[0])
			}
		}
	default:
		return fmt.Errorf("unknown command %q", c.Name)
	}
	return nil
}

func plural(min, max int) string {
	switch {
	case min == max && min == 1:
		return "exactly one argument"
	case min == max:
		return fmt.Sprintf("exactly %d arguments", min)
	case max == 1<<10:
		return "at least one argument"
	}
	return fmt.Sprintf("%d to %d arguments", min, max)
}

// Run executes the commands in order. It returns *ExitError for an exit command, nil
// when the script simply ends, and an error naming the line for anything that fails.
func Run(ctx context.Context, cmds []Command, env Env) error {
	if env.Dir == "" {
		env.Dir, _ = os.Getwd()
	}
	if env.TicketDir == "" {
		env.TicketDir = filepath.Dir(env.Dir)
	}
	if env.InputIdle == 0 {
		env.InputIdle = 300 * time.Millisecond
	}
	if env.Stdout == nil {
		env.Stdout = io.Discard
	}
	r := &runner{env: env}
	for _, c := range cmds {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.exec(ctx, c)
		var exit *ExitError
		if errors.As(err, &exit) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil {
			return fmt.Errorf("script line %d (%s): %w", c.Line, c.Name, err)
		}
	}
	return nil
}

type runner struct {
	env   Env
	lines chan inputLine // started on first wait-input
}

type inputLine struct {
	text string
	err  error
}

func (r *runner) exec(ctx context.Context, c Command) error {
	switch c.Name {
	case "write":
		return r.write(c.Args[0], c.Args[1])
	case "commit":
		return r.commit(ctx, c.Args[0])
	case "append-proposal":
		why := ""
		if len(c.Args) == 2 {
			why = c.Args[1]
		}
		return r.appendProposal(c.Args[0], why)
	case "state":
		fmt.Fprintf(r.env.Stdout, "[fakeagent] state=%s\n", c.Args[0])
		if r.env.ReportState != nil {
			return r.env.ReportState(c.Args[0])
		}
	case "say":
		fmt.Fprintln(r.env.Stdout, strings.Join(c.Args, " "))
	case "sleep":
		d, _ := time.ParseDuration(c.Args[0])
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	case "wait-input":
		return r.waitInput(ctx, c.Args[0])
	case "exit":
		code := 0
		if len(c.Args) == 1 {
			code, _ = strconv.Atoi(c.Args[0])
		}
		return &ExitError{Code: code}
	}
	return nil
}

// resolve makes path absolute; a relative one may not climb out of the working directory.
func (r *runner) resolve(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	full := filepath.Join(r.env.Dir, path)
	if rel, err := filepath.Rel(r.env.Dir, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the working directory", path)
	}
	return full, nil
}

func (r *runner) write(path, content string) error {
	full, err := r.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func (r *runner) commit(ctx context.Context, message string) error {
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "core.hooksPath=" + os.DevNull, "commit", "-q", "-m", message},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = r.env.Dir
		// Environment variables outrank any -c user.* setting, so the identity goes here.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=fakeagent", "GIT_AUTHOR_EMAIL=fakeagent@rig.invalid",
			"GIT_COMMITTER_NAME=fakeagent", "GIT_COMMITTER_EMAIL=fakeagent@rig.invalid")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[len(args)-2], err, strings.TrimSpace(out.String()))
		}
	}
	return nil
}

var proposalHeader = regexp.MustCompile(`(?m)^### P-([a-z][a-z0-9-]*)-(\d+) · from `)

// appendProposal writes a proposal the way an agent should, never touching ## Agreed:
// under "## Proposed changes", numbered per service so agents cannot collide.
func (r *runner) appendProposal(what, why string) error {
	if r.env.Service == "" {
		return errors.New("no service name to propose as (set RIG_SERVICE)")
	}
	path := filepath.Join(r.env.TicketDir, "contract.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no contract.md in %s: %w", r.env.TicketDir, err)
	}
	doc := string(raw)

	n := 1
	for _, m := range proposalHeader.FindAllStringSubmatch(doc, -1) {
		if m[1] == r.env.Service {
			if k, _ := strconv.Atoi(m[2]); k >= n {
				n = k + 1
			}
		}
	}
	block := fmt.Sprintf("### P-%s-%d · from %s · open\n**What:** %s\n", r.env.Service, n, r.env.Service, what)
	if why != "" {
		block += fmt.Sprintf("**Why:** %s\n", why)
	}

	const section = "## Proposed changes"
	start := strings.Index(doc, section)
	if start < 0 {
		at := len(doc)
		if i := strings.Index(doc, "\n## Changelog"); i >= 0 {
			at = i + 1
		}
		doc = doc[:at] + ensureBlankLine(doc[:at]) + section + "\n\n" + block + "\n" + doc[at:]
		return os.WriteFile(path, []byte(doc), 0o644)
	}
	end := len(doc)
	if i := strings.Index(doc[start+len(section):], "\n## "); i >= 0 {
		end = start + len(section) + i + 1
	}
	head := strings.TrimRight(doc[:end], "\n")
	doc = head + "\n\n" + block + "\n" + doc[end:]
	return os.WriteFile(path, []byte(doc), 0o644)
}

func ensureBlankLine(s string) string {
	switch {
	case s == "" || strings.HasSuffix(s, "\n\n"):
		return ""
	case strings.HasSuffix(s, "\n"):
		return "\n"
	}
	return "\n\n"
}

// waitInput blocks until a line arrives, then keeps taking lines until input goes quiet
// for InputIdle, so a multi-line message typed into the terminal is recorded whole.
func (r *runner) waitInput(ctx context.Context, file string) error {
	full, err := r.resolve(file)
	if err != nil {
		return err
	}
	if r.lines == nil {
		r.lines = make(chan inputLine, 1024)
		in := r.env.Stdin
		if in == nil {
			in = strings.NewReader("")
		}
		go func() {
			br := bufio.NewReader(in)
			for {
				text, err := br.ReadString('\n')
				if text != "" {
					r.lines <- inputLine{text: text}
				}
				if err != nil {
					r.lines <- inputLine{err: err}
					return
				}
			}
		}()
	}

	var got strings.Builder
	first := true
	for {
		var timeout <-chan time.Time
		if !first {
			timeout = time.After(r.env.InputIdle)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return r.record(full, got.String())
		case l := <-r.lines:
			if l.err != nil {
				if first {
					return errors.New("input ended before any arrived")
				}
				return r.record(full, got.String())
			}
			got.WriteString(l.text)
			first = false
		}
	}
}

func (r *runner) record(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}
