// Package tmux is the terminal.Backend for tmux, on a private server socket so rig
// never touches the user's own tmux. A ticket is a tmux session and each service is a
// window in it; finished panes are kept (remain-on-exit) so their exit status can be
// read (docs/spikes/03-tmux.md §1).
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

type Options struct {
	Socket string // the private server's -L name (default "rig")
	Binary string // the tmux executable (default "tmux")
}

type Backend struct {
	opts    Options
	spawnMu sync.Mutex // so two spawns never race to create the ticket's session
	buffers atomic.Int64
}

var _ terminal.Backend = (*Backend)(nil)

func New(o Options) *Backend {
	if o.Socket == "" {
		o.Socket = "rig"
	}
	if o.Binary == "" {
		o.Binary = "tmux"
	}
	return &Backend{opts: o}
}

// -e on new-session needs tmux 3.2.
const minMajor, minMinor = 3, 2

func (b *Backend) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	binary, err := exec.LookPath(b.opts.Binary)
	if err != nil {
		return fmt.Errorf("%w: tmux is not installed or not on PATH (%v)", terminal.ErrUnavailable, err)
	}
	out, err := exec.CommandContext(ctx, binary, "-V").Output()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: tmux -V: %v", terminal.ErrUnavailable, err)
	}
	if !atLeast(string(out), minMajor, minMinor) {
		return fmt.Errorf("%w: rig needs tmux %d.%d or newer, found %q", terminal.ErrUnavailable, minMajor, minMinor, strings.TrimSpace(string(out)))
	}
	return nil
}

// run executes one tmux command line on the private server. -f /dev/null keeps the
// user's tmux.conf (prefix, base-index, hooks) out of rig's server.
func (b *Backend) run(ctx context.Context, stdin string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	binary, err := exec.LookPath(b.opts.Binary)
	if err != nil {
		return "", fmt.Errorf("%w: tmux is not installed or not on PATH (%v)", terminal.ErrUnavailable, err)
	}
	cmd := exec.CommandContext(ctx, binary, append([]string{"-f", "/dev/null", "-L", b.opts.Socket}, args...)...)
	cmd.Env = withoutTMUX(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", mapError(strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

func withoutTMUX(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "TMUX=") && !strings.HasPrefix(e, "TMUX_PANE=") {
			out = append(out, e)
		}
	}
	return out
}

var errNoServer = errors.New("no tmux server running")

func mapError(stderr string, err error) error {
	switch {
	case strings.HasPrefix(stderr, "no server running"), strings.HasPrefix(stderr, "error connecting to"):
		return errNoServer
	case strings.HasPrefix(stderr, "can't find"):
		return fmt.Errorf("%w: %s", terminal.ErrNotFound, stderr)
	}
	return fmt.Errorf("tmux: %s (%v)", stderr, err)
}

const paneFormat = "#{pane_id}\t#{session_name}\t#{window_name}\t#{pane_dead}\t#{pane_dead_status}"

func (b *Backend) panes(ctx context.Context) ([]terminal.Pane, error) {
	out, err := b.run(ctx, "", "list-panes", "-a", "-F", paneFormat)
	if errors.Is(err, errNoServer) {
		return nil, nil // no server means no panes
	}
	if err != nil {
		return nil, err
	}
	var panes []terminal.Pane
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			return nil, fmt.Errorf("tmux: unexpected list-panes line %q", line)
		}
		p := terminal.Pane{ID: terminal.PaneID(f[0]), Ticket: f[1], Service: f[2], Alive: f[3] != "1", Heuristic: domain.AgentUnknown}
		if !p.Alive {
			if code, err := strconv.Atoi(f[4]); err == nil {
				p.ExitStatus = &code
			}
		}
		panes = append(panes, p)
	}
	sort.Slice(panes, func(i, j int) bool { return paneNumber(panes[i].ID) < paneNumber(panes[j].ID) })
	return panes, nil
}

var paneID = regexp.MustCompile(`^%[0-9]+$`)

func paneNumber(id terminal.PaneID) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(string(id), "%"))
	return n
}

func (b *Backend) List(ctx context.Context) ([]terminal.Pane, error) {
	panes, err := b.panes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range panes {
		if panes[i].Alive {
			panes[i].Heuristic = b.guess(ctx, panes[i].ID)
		}
	}
	return panes, nil
}

func (b *Backend) Get(ctx context.Context, id terminal.PaneID) (terminal.Pane, error) {
	if !paneID.MatchString(string(id)) {
		return terminal.Pane{}, fmt.Errorf("%w: %q", terminal.ErrNotFound, id)
	}
	panes, err := b.panes(ctx)
	if err != nil {
		return terminal.Pane{}, err
	}
	for _, p := range panes {
		if p.ID == id {
			if p.Alive {
				p.Heuristic = b.guess(ctx, id)
			}
			return p, nil
		}
	}
	return terminal.Pane{}, fmt.Errorf("%w: %s", terminal.ErrNotFound, id)
}

func (b *Backend) guess(ctx context.Context, id terminal.PaneID) domain.AgentState {
	screen, err := b.run(ctx, "", "capture-pane", "-p", "-t", string(id))
	if err != nil {
		return domain.AgentUnknown
	}
	return heuristic(screen)
}

func (b *Backend) Spawn(ctx context.Context, spec terminal.SpawnSpec) (terminal.PaneID, error) {
	switch {
	case spec.Ticket == "" || spec.Service == "":
		return "", fmt.Errorf("%w: a ticket and a service are required", terminal.ErrInvalid)
	case strings.ContainsAny(spec.Ticket, ".:"):
		return "", fmt.Errorf("%w: ticket %q cannot contain '.' or ':' (tmux session names)", terminal.ErrInvalid, spec.Ticket)
	case !filepath.IsAbs(spec.Dir):
		return "", fmt.Errorf("%w: directory %q must be absolute", terminal.ErrInvalid, spec.Dir)
	}
	if info, err := os.Stat(spec.Dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: directory %s does not exist", terminal.ErrInvalid, spec.Dir)
	}
	var envArgs []string
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !envName.MatchString(k) {
			return "", fmt.Errorf("%w: environment variable name %q", terminal.ErrInvalid, k)
		}
		envArgs = append(envArgs, "-e", k+"="+spec.Env[k])
	}
	if err := b.Ready(ctx); err != nil {
		return "", err
	}

	b.spawnMu.Lock()
	defer b.spawnMu.Unlock()
	existing, err := b.panes(ctx)
	if err != nil {
		return "", err
	}
	haveSession := false
	for _, p := range existing {
		if p.Ticket == spec.Ticket {
			haveSession = true
			if p.Service == spec.Service {
				return "", fmt.Errorf("%w: %s/%s", terminal.ErrExists, spec.Ticket, spec.Service)
			}
		}
	}

	// A command given as separate arguments is run directly, without a shell, so no
	// quoting is needed. remain-on-exit is set in the same invocation: tmux handles
	// the exit of a process only after the whole command line has run, so even an
	// instantly-exiting command is kept.
	create := []string{"new-window", "-d", "-t", "=" + spec.Ticket + ":"}
	if !haveSession {
		create = []string{"new-session", "-d", "-s", spec.Ticket, "-x", "200", "-y", "50"}
	}
	args := append(create, "-n", spec.Service, "-c", spec.Dir, "-P", "-F", "#{pane_id}")
	args = append(args, envArgs...)
	args = append(args, spec.Command...)
	args = append(args, ";", "set-option", "-g", "remain-on-exit", "on")
	out, err := b.run(ctx, "", args...)
	if err != nil {
		return "", fmt.Errorf("start %s/%s: %w", spec.Ticket, spec.Service, err)
	}
	return terminal.PaneID(strings.TrimSpace(out)), nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// live returns the pane, or ErrNotFound if it is unknown or its process has ended
// (typing into a dead pane would go nowhere).
func (b *Backend) live(ctx context.Context, id terminal.PaneID) error {
	panes, err := b.panes(ctx)
	if err != nil {
		return err
	}
	for _, p := range panes {
		if p.ID == id {
			if !p.Alive {
				return fmt.Errorf("%w: %s has exited", terminal.ErrNotFound, id)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s", terminal.ErrNotFound, id)
}

func (b *Backend) Read(ctx context.Context, id terminal.PaneID, lines int) (string, error) {
	if _, err := b.Get(ctx, id); err != nil {
		return "", err
	}
	args := []string{"capture-pane", "-p", "-t", string(id)}
	if lines > 0 {
		args = append(args, "-S", "-")
	}
	out, err := b.run(ctx, "", args...)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		return out, nil
	}
	all := strings.Split(strings.TrimRight(out, "\n \t"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n") + "\n", nil
}

// SendText types literally. Multi-line or large text goes in as a bracketed paste so
// an agent TUI takes it as one block instead of submitting at each newline.
func (b *Backend) SendText(ctx context.Context, id terminal.PaneID, text string) error {
	if !paneID.MatchString(string(id)) {
		return fmt.Errorf("%w: %q", terminal.ErrNotFound, id)
	}
	if err := b.live(ctx, id); err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	if !strings.Contains(text, "\n") && len(text) <= 512 {
		_, err := b.run(ctx, "", "send-keys", "-l", "-t", string(id), "--", text)
		return err
	}
	buffer := fmt.Sprintf("rig-%d-%d", os.Getpid(), b.buffers.Add(1))
	if _, err := b.run(ctx, text, "load-buffer", "-b", buffer, "-"); err != nil {
		return err
	}
	_, err := b.run(ctx, "", "paste-buffer", "-p", "-d", "-b", buffer, "-t", string(id))
	return err
}

func (b *Backend) Submit(ctx context.Context, id terminal.PaneID) error {
	if !paneID.MatchString(string(id)) {
		return fmt.Errorf("%w: %q", terminal.ErrNotFound, id)
	}
	if err := b.live(ctx, id); err != nil {
		return err
	}
	_, err := b.run(ctx, "", "send-keys", "-t", string(id), "Enter")
	return err
}

func (b *Backend) Close(ctx context.Context, id terminal.PaneID) error {
	if !paneID.MatchString(string(id)) {
		return fmt.Errorf("%w: %q", terminal.ErrNotFound, id)
	}
	if _, err := b.Get(ctx, id); err != nil {
		return err
	}
	_, err := b.run(ctx, "", "kill-pane", "-t", string(id))
	if errors.Is(err, errNoServer) { // that was the last pane and the server went with it
		return nil
	}
	return err
}

func (b *Backend) Attach(ctx context.Context, id terminal.PaneID) ([]string, error) {
	p, err := b.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	b.run(ctx, "", "select-window", "-t", string(id)) // best effort: show the service's window first
	return []string{b.opts.Binary, "-L", b.opts.Socket, "attach", "-t", "=" + p.Ticket}, nil
}
