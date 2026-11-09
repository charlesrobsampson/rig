// Package client is the one way rig commands talk to the daemon: connect (starting
// the daemon on demand), handshake, call, and stream.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
)

const (
	defaultStartupTimeout = 5 * time.Second
	defaultPollInterval   = 50 * time.Millisecond
)

// Spawner starts the daemon in the background and returns without waiting for it.
type Spawner func(ctx context.Context) error

type Options struct {
	SocketPath string
	Version    string // sent in hello
	// Spawner starts the daemon when none is running; nil turns auto-start off.
	Spawner        Spawner
	Clock          clock.Clock // nil means the real clock
	StartupTimeout time.Duration
	PollInterval   time.Duration
	// StartHint is appended to the "did not start" error, e.g. where to find the log.
	StartHint string
}

// Client is one connection to the daemon. Calls are serialized; open a dedicated
// Client for each stream. After a cancelled call or a lost connection it is unusable.
type Client struct {
	opts   Options
	conn   net.Conn
	enc    *api.Encoder
	dec    *api.Decoder
	mu     sync.Mutex
	seq    int
	broken bool
}

func Connect(ctx context.Context, opts Options) (*Client, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = defaultStartupTimeout
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = defaultPollInterval
	}

	c, err := open(ctx, opts)
	if err == nil || !isNotRunning(err) || opts.Spawner == nil {
		return c, err
	}
	if err := opts.Spawner(ctx); err != nil {
		return nil, &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("start daemon: %v", err)}
	}
	deadline := opts.Clock.After(opts.StartupTimeout)
	for {
		if c, err = open(ctx, opts); err == nil || !isNotRunning(err) {
			return c, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			msg := fmt.Sprintf("daemon did not start within %s", opts.StartupTimeout)
			if opts.StartHint != "" {
				msg += "; " + opts.StartHint
			}
			return nil, &api.Error{Code: api.CodeUnavailable, Message: msg}
		case <-opts.Clock.After(opts.PollInterval):
		}
	}
}

func open(ctx context.Context, opts Options) (*Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", opts.SocketPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("daemon is not running (nothing is listening on %s)", opts.SocketPath)}
		}
		return nil, &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("connect to daemon: %v", err)}
	}
	c := &Client{opts: opts, conn: conn, enc: api.NewEncoder(conn), dec: api.NewDecoder(conn)}
	var hello api.HelloResult
	err = c.Call(ctx, api.MethodHello, api.HelloParams{Client: "rig", Version: opts.Version}, &hello)
	if err == nil {
		if err = hello.Compatible(); err != nil {
			err = &api.Error{Code: api.CodeUnavailable, Message: err.Error()}
		}
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func isNotRunning(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.Code == api.CodeNotRunning
}

func (c *Client) Close() error { return c.conn.Close() }

// Call sends one request and decodes its result into result (nil to ignore it).
// A failure from the daemon is returned as an *api.Error.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer stop()

	id, err := c.send(method, params)
	if err != nil {
		return c.fail(ctx, err)
	}
	resp, err := c.dec.ReadResponse()
	if err != nil {
		return c.fail(ctx, err)
	}
	if resp.ID != id {
		c.broken = true
		return fmt.Errorf("unexpected response id %q (wanted %q)", resp.ID, id)
	}
	if err := resp.Err(); err != nil {
		return err
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

// Stream calls a streaming method, passing each item to onItem until the daemon ends
// the stream, onItem fails, or ctx is cancelled. The Client cannot be reused after.
func (c *Client) Stream(ctx context.Context, method string, params any, onItem func(json.RawMessage) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer stop()
	defer func() { c.broken = true }()

	id, err := c.send(method, params)
	if err != nil {
		return c.fail(ctx, err)
	}
	for {
		resp, err := c.dec.ReadResponse()
		if err != nil {
			return c.fail(ctx, err)
		}
		switch {
		case resp.ID != id:
			return fmt.Errorf("unexpected response id %q (wanted %q)", resp.ID, id)
		case resp.Err() != nil:
			return resp.Err()
		case resp.IsEnd():
			return nil
		case resp.IsStream():
			if err := onItem(resp.Stream); err != nil {
				return err
			}
		}
	}
}

func (c *Client) Status(ctx context.Context) (api.StatusResult, error) {
	var status api.StatusResult
	err := c.Call(ctx, api.MethodStatus, nil, &status)
	return status, err
}

// Stop asks the daemon to shut down and returns once its socket is gone.
func (c *Client) Stop(ctx context.Context) error {
	if err := c.Call(ctx, api.MethodDaemonStop, nil, nil); err != nil {
		return err
	}
	c.broken = true
	deadline := c.opts.Clock.After(c.opts.StartupTimeout)
	for {
		if _, err := os.Stat(c.opts.SocketPath); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("daemon did not stop within %s", c.opts.StartupTimeout)}
		case <-c.opts.Clock.After(c.opts.PollInterval):
		}
	}
}

// begin arms the context on the connection: its deadline becomes the I/O deadline,
// and cancellation interrupts a blocked read. The returned func disarms it.
func (c *Client) begin(ctx context.Context) (func(), error) {
	if c.broken {
		return nil, &api.Error{Code: api.CodeNotRunning, Message: "daemon connection is closed"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	c.conn.SetDeadline(deadline)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.conn.SetDeadline(time.Unix(1, 0))
		case <-done:
		}
	}()
	return func() { close(done) }, nil
}

func (c *Client) send(method string, params any) (string, error) {
	c.seq++
	id := strconv.Itoa(c.seq)
	req := api.Request{V: api.ProtocolVersion, ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return "", fmt.Errorf("encode %s params: %w", method, err)
		}
		req.Params = raw
	}
	return id, c.enc.Write(req)
}

// fail turns an I/O error into the right error and retires the connection, since
// an interrupted exchange leaves it in an unknown state.
func (c *Client) fail(ctx context.Context, err error) error {
	c.broken = true
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// The socket deadline is the context's, and it can fire a moment before the
	// context itself reports being expired.
	if deadline, ok := ctx.Deadline(); ok && errors.Is(err, os.ErrDeadlineExceeded) && !time.Now().Before(deadline.Add(-50*time.Millisecond)) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return &api.Error{Code: api.CodeNotRunning, Message: "daemon connection lost"}
	}
	return err
}
