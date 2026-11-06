package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var epoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// shortDir keeps unix socket paths under macOS's ~104-character limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type harness struct {
	srv    *daemon.Server
	opts   daemon.Options
	clock  *testkit.FakeClock
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

func options(dir string, c *testkit.FakeClock) daemon.Options {
	return daemon.Options{
		SocketPath: filepath.Join(dir, "rigd.sock"),
		PIDPath:    filepath.Join(dir, "rigd.pid"),
		Version:    "test",
		Clock:      c,
	}
}

func start(t *testing.T, configure func(*daemon.Server)) *harness {
	t.Helper()
	clock := testkit.NewFakeClock(epoch)
	opts := options(shortDir(t), clock)
	srv, err := daemon.Listen(opts)
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		configure(srv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{srv: srv, opts: opts, clock: clock, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		h.cancel()
		h.wait(t)
	})
	return h
}

// wait returns Serve's result once it has returned, failing the test if it hangs.
func (h *harness) wait(t *testing.T) error {
	t.Helper()
	h.once.Do(func() {
		select {
		case h.err = <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down")
		}
	})
	return h.err
}

type client struct {
	conn net.Conn
	enc  *api.Encoder
	dec  *api.Decoder
	seq  int
}

func dial(t *testing.T, path string) *client {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return &client{conn: conn, enc: api.NewEncoder(conn), dec: api.NewDecoder(conn)}
}

func (c *client) send(t *testing.T, method string, params any) string {
	t.Helper()
	c.seq++
	id := strconv.Itoa(c.seq)
	req := api.Request{V: api.ProtocolVersion, ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		req.Params = raw
	}
	if err := c.enc.Write(req); err != nil {
		t.Fatal(err)
	}
	return id
}

func (c *client) call(t *testing.T, method string, params any) api.Response {
	t.Helper()
	id := c.send(t, method, params)
	resp, err := c.dec.ReadResponse()
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	if resp.ID != id {
		t.Fatalf("%s: response id %q, want %q", method, resp.ID, id)
	}
	return resp
}

func (c *client) hello(t *testing.T) {
	t.Helper()
	resp := c.call(t, api.MethodHello, api.HelloParams{Client: "test", Version: "x"})
	if resp.Err() != nil {
		t.Fatalf("hello failed: %v", resp.Err())
	}
}

func wantError(t *testing.T, resp api.Response, code api.ErrorCode, substr string) {
	t.Helper()
	var e *api.Error
	if !errors.As(resp.Err(), &e) || e.Code != code || !strings.Contains(e.Message, substr) {
		t.Fatalf("response error = %v, want %s mentioning %q", resp.Err(), code, substr)
	}
}

func TestHelloAndStatus(t *testing.T) {
	h := start(t, nil)
	c := dial(t, h.opts.SocketPath)

	resp := c.call(t, api.MethodHello, api.HelloParams{Client: "test", Version: "x"})
	var hello api.HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil || hello.Protocol != api.ProtocolVersion || !strings.Contains(hello.Server, "test") {
		t.Fatalf("hello = %+v, %v", hello, err)
	}

	h.clock.Advance(90 * time.Second)
	resp = c.call(t, api.MethodStatus, nil)
	var status api.StatusResult
	if err := json.Unmarshal(resp.Result, &status); err != nil {
		t.Fatal(err)
	}
	if status.Version != "test" || status.PID != os.Getpid() || status.UptimeSeconds != 90 {
		t.Errorf("status = %+v", status)
	}
}

func TestTheFirstRequestMustBeHello(t *testing.T) {
	h := start(t, nil)
	c := dial(t, h.opts.SocketPath)
	wantError(t, c.call(t, api.MethodStatus, nil), api.CodeInvalid, "hello")
	c.hello(t)
	if resp := c.call(t, api.MethodStatus, nil); resp.Err() != nil {
		t.Errorf("status after hello failed: %v", resp.Err())
	}
}

func TestBadRequestsGetStructuredErrorsAndTheConnectionSurvives(t *testing.T) {
	h := start(t, nil)
	c := dial(t, h.opts.SocketPath)
	c.hello(t)

	tests := []struct {
		name string
		do   func() api.Response
		code api.ErrorCode
		text string
	}{
		{"unknown method", func() api.Response { return c.call(t, "ticket.explode", nil) }, api.CodeInvalid, "ticket.explode"},
		{"documented method nobody handles yet", func() api.Response { return c.call(t, api.MethodTicketStart, nil) }, api.CodeInvalid, "not implemented"},
		{"wrong protocol version", func() api.Response {
			c.conn.Write([]byte(`{"v":2,"id":"v2","method":"status"}` + "\n"))
			r, _ := c.dec.ReadResponse()
			return r
		}, api.CodeInvalid, "version 2"},
		{"malformed json", func() api.Response {
			c.conn.Write([]byte("{this is not json}\n"))
			r, _ := c.dec.ReadResponse()
			return r
		}, api.CodeInvalid, "decode"},
		{"oversized line", func() api.Response {
			c.conn.Write([]byte(`{"v":1,"id":"big","method":"status","params":"` + strings.Repeat("a", api.MaxLineBytes) + `"}` + "\n"))
			r, _ := c.dec.ReadResponse()
			return r
		}, api.CodeInvalid, "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { wantError(t, tt.do(), tt.code, tt.text) })
	}
	if resp := c.call(t, api.MethodStatus, nil); resp.Err() != nil {
		t.Errorf("the connection did not survive the bad requests: %v", resp.Err())
	}
}

type echoParams struct {
	Text string `json:"text"`
}

func TestRegisteredHandlers(t *testing.T) {
	h := start(t, func(s *daemon.Server) {
		s.Handle(api.MethodTicketGet, func(_ context.Context, params json.RawMessage) (any, error) {
			var p echoParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &api.Error{Code: api.CodeInvalid, Message: "bad params"}
			}
			return echoParams{Text: strings.ToUpper(p.Text)}, nil
		})
		s.Handle(api.MethodTicketStop, func(context.Context, json.RawMessage) (any, error) {
			return nil, &api.Error{Code: api.CodeBlocked, Message: "dirty checkout", Details: []string{"api"}}
		})
		s.Handle(api.MethodTicketNew, func(context.Context, json.RawMessage) (any, error) {
			return nil, errors.New("disk exploded")
		})
		s.Handle(api.MethodGateList, func(context.Context, json.RawMessage) (any, error) {
			panic("handler bug")
		})
	})
	c := dial(t, h.opts.SocketPath)
	c.hello(t)

	resp := c.call(t, api.MethodTicketGet, echoParams{Text: "hi"})
	var got echoParams
	if err := json.Unmarshal(resp.Result, &got); err != nil || got.Text != "HI" {
		t.Errorf("result = %s, %v", resp.Result, err)
	}

	resp = c.call(t, api.MethodTicketStop, nil)
	var apiErr *api.Error
	if !errors.As(resp.Err(), &apiErr) || apiErr.Code != api.CodeBlocked || len(apiErr.Details) != 1 || apiErr.Details[0] != "api" {
		t.Errorf("a handler's *api.Error must reach the client unchanged, got %v", resp.Err())
	}

	wantError(t, c.call(t, api.MethodTicketNew, nil), api.CodeInternal, "disk exploded")
	wantError(t, c.call(t, api.MethodGateList, nil), api.CodeInternal, "panic")
	if resp := c.call(t, api.MethodStatus, nil); resp.Err() != nil {
		t.Errorf("the daemon did not survive a handler panic: %v", resp.Err())
	}
}

func TestStreamingHandlers(t *testing.T) {
	cancelled := make(chan struct{})
	h := start(t, func(s *daemon.Server) {
		s.HandleStream(api.MethodLogsStream, func(_ context.Context, _ json.RawMessage, send func(any) error) error {
			for i := 1; i <= 3; i++ {
				if err := send(map[string]int{"n": i}); err != nil {
					return err
				}
			}
			return nil
		})
		s.HandleStream(api.MethodEventsSubscribe, func(ctx context.Context, _ json.RawMessage, send func(any) error) error {
			send(map[string]string{"hello": "subscriber"})
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		})
	})

	t.Run("items then an end marker", func(t *testing.T) {
		c := dial(t, h.opts.SocketPath)
		c.hello(t)
		id := c.send(t, api.MethodLogsStream, nil)
		var items int
		for {
			r, err := c.dec.ReadResponse()
			if err != nil {
				t.Fatal(err)
			}
			if r.ID != id {
				t.Fatalf("id = %q, want %q", r.ID, id)
			}
			if r.IsEnd() {
				break
			}
			if !r.IsStream() {
				t.Fatalf("unexpected message %+v", r)
			}
			items++
		}
		if items != 3 {
			t.Errorf("items = %d, want 3", items)
		}
	})

	t.Run("the handler is cancelled when the client disconnects", func(t *testing.T) {
		c := dial(t, h.opts.SocketPath)
		c.hello(t)
		c.send(t, api.MethodEventsSubscribe, nil)
		if _, err := c.dec.ReadResponse(); err != nil {
			t.Fatal(err)
		}
		c.conn.Close()
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("stream handler kept running after the client left")
		}
	})
}

func TestConcurrentClients(t *testing.T) {
	h := start(t, func(s *daemon.Server) {
		s.Handle(api.MethodTicketGet, func(_ context.Context, params json.RawMessage) (any, error) {
			var p echoParams
			json.Unmarshal(params, &p)
			return p, nil
		})
	})
	const clients = 25
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("unix", h.opts.SocketPath)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			c := &client{conn: conn, enc: api.NewEncoder(conn), dec: api.NewDecoder(conn)}
			if resp := c.call(t, api.MethodHello, api.HelloParams{Client: "c"}); resp.Err() != nil {
				t.Error(resp.Err())
				return
			}
			want := fmt.Sprintf("client-%d", i)
			resp := c.call(t, api.MethodTicketGet, echoParams{Text: want})
			var got echoParams
			if err := json.Unmarshal(resp.Result, &got); err != nil || got.Text != want {
				t.Errorf("client %d got %s, %v", i, resp.Result, err)
			}
		}()
	}
	wg.Wait()
}

func TestASecondDaemonOnTheSameSocketFailsAndTheFirstKeepsWorking(t *testing.T) {
	h := start(t, nil)
	_, err := daemon.Listen(h.opts)
	if err == nil {
		t.Fatal("a second daemon started on the same socket")
	}
	for _, want := range []string{"already running", strconv.Itoa(os.Getpid()), h.opts.PIDPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	c := dial(t, h.opts.SocketPath)
	c.hello(t)
	if resp := c.call(t, api.MethodStatus, nil); resp.Err() != nil {
		t.Errorf("the running daemon was disturbed: %v", resp.Err())
	}
}

func TestStalePIDFileAndSocketFromACrashAreRecovered(t *testing.T) {
	dir := shortDir(t)
	opts := options(dir, testkit.NewFakeClock(epoch))

	// What a crashed daemon leaves behind: a pid file naming a dead process and a
	// socket file nobody is listening on.
	if err := os.WriteFile(opts.PIDPath, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", opts.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(opts.SocketPath); err != nil {
		t.Fatalf("test setup: the stale socket is gone: %v", err)
	}

	srv, err := daemon.Listen(opts)
	if err != nil {
		t.Fatalf("a stale pid file and socket must not block startup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	defer func() { cancel(); <-done }()

	c := dial(t, opts.SocketPath)
	c.hello(t)
	var status api.StatusResult
	json.Unmarshal(c.call(t, api.MethodStatus, nil).Result, &status)
	if status.PID != os.Getpid() {
		t.Errorf("pid = %d, want this process's %d", status.PID, os.Getpid())
	}
}

func TestListenRefusesToDeleteAFileThatIsNotASocket(t *testing.T) {
	opts := options(shortDir(t), testkit.NewFakeClock(epoch))
	if err := os.WriteFile(opts.SocketPath, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Listen(opts); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("error = %v, want one saying the path is not a socket", err)
	}
	if got, _ := os.ReadFile(opts.SocketPath); string(got) != "precious" {
		t.Error("the file was modified")
	}
}

func TestShutdownClosesConnectionsAndCleansUp(t *testing.T) {
	h := start(t, nil)
	c := dial(t, h.opts.SocketPath)
	c.hello(t)

	h.cancel()
	if err := h.wait(t); err != nil {
		t.Fatalf("Serve returned %v", err)
	}

	if _, err := c.dec.ReadResponse(); !errors.Is(err, io.EOF) {
		t.Errorf("open connection after shutdown: %v, want EOF", err)
	}
	for _, path := range []string{h.opts.SocketPath, h.opts.PIDPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after shutdown", path)
		}
	}
	srv, err := daemon.Listen(h.opts)
	if err != nil {
		t.Fatalf("a new daemon cannot start after a clean shutdown: %v", err)
	}
	srv.Stop()
	srv.Serve(context.Background())
}

func TestStopShutsTheServerDownLikeCancellation(t *testing.T) {
	h := start(t, nil)
	h.srv.Stop()
	if err := h.wait(t); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	h := start(t, nil)
	info, err := os.Stat(h.opts.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("socket mode = %v, want no group/other access", info.Mode().Perm())
	}
}

func TestOverlongSocketPathsAreRejectedClearly(t *testing.T) {
	long := filepath.Join(shortDir(t), strings.Repeat("d", 120), "rigd.sock")
	_, err := daemon.Listen(daemon.Options{SocketPath: long, PIDPath: long + ".pid", Version: "t"})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("error = %v, want one saying the socket path is too long", err)
	}
}
