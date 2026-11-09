// Package dockercli is the docker.Docker that drives the `docker` command. Using the
// CLI keeps rig a small, plain Go binary (no SDK and its API-version churn), and it
// honors whatever the user's Docker setup already says: contexts, DOCKER_HOST, Docker
// Desktop, corporate proxies. Secrets never appear in the command line: environment
// values travel in the docker process's own environment (`-e NAME`).
package dockercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/charlesrobsampson/rig/internal/docker"
)

// Call is one invocation of the docker command.
type Call struct {
	Args []string
	// Env is added to the process's environment (used to pass secret values).
	Env   map[string]string
	Stdin io.Reader
}

// Result is what a finished invocation produced. A non-zero ExitCode is not a Go
// error: the caller maps stderr to the right docker error.
type Result struct {
	Stdout, Stderr string
	// Output is stdout and stderr interleaved as they were written.
	Output   string
	ExitCode int
}

// Runner executes a Call. It returns an error only when docker could not be run at all
// or the context ended.
type Runner func(ctx context.Context, c Call) (Result, error)

// Streamer starts a long-running call (following logs) and returns its merged output.
// Closing the reader ends the process.
type Streamer func(ctx context.Context, args []string) (io.ReadCloser, error)

type Options struct {
	Binary   string   // default "docker"
	Runner   Runner   // nil means run the binary
	Streamer Streamer // nil means run the binary
}

type Client struct {
	opts Options
}

var _ docker.Docker = (*Client)(nil)

func New(o Options) *Client {
	if o.Binary == "" {
		o.Binary = "docker"
	}
	c := &Client{opts: o}
	if c.opts.Runner == nil {
		c.opts.Runner = c.exec
	}
	if c.opts.Streamer == nil {
		c.opts.Streamer = c.stream
	}
	return c
}

var errNoBinary = "docker is not installed or not on PATH"

func (c *Client) exec(ctx context.Context, call Call) (Result, error) {
	binary, err := exec.LookPath(c.opts.Binary)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s", docker.ErrUnavailable, errNoBinary)
	}
	cmd := exec.CommandContext(ctx, binary, call.Args...)
	cmd.Env = withEnv(os.Environ(), call.Env)
	cmd.Stdin = call.Stdin
	var stdout, stderr, both bytes.Buffer
	merged := &syncWriter{w: &both}
	cmd.Stdout = io.MultiWriter(&stdout, merged)
	cmd.Stderr = io.MultiWriter(&stderr, merged)
	err = cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), Output: both.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return Result{}, ctx.Err()
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return Result{}, fmt.Errorf("run docker: %w", err)
	}
	return res, nil
}

func withEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !hasKey(extra, k) {
			out = append(out, kv)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func hasKey(m map[string]string, k string) bool { _, ok := m[k]; return ok }

// run calls docker and turns a failed call into the matching docker error.
func (c *Client) run(ctx context.Context, args ...string) (Result, error) {
	return c.runCall(ctx, Call{Args: args})
}

func (c *Client) runCall(ctx context.Context, call Call) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	res, err := c.opts.Runner(ctx, call)
	if err != nil {
		return Result{}, err
	}
	if res.ExitCode != 0 {
		return res, mapError(res.Stderr + res.Output)
	}
	return res, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.run(ctx, "version", "--format", "{{.Server.Version}}")
	return err
}
