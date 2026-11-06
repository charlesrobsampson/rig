package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/client"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type paths struct{ socket, pid string }

func newPaths(t *testing.T) paths {
	dir := shortDir(t)
	return paths{socket: filepath.Join(dir, "rigd.sock"), pid: filepath.Join(dir, "rigd.pid")}
}

// startDaemon runs a real in-process daemon and registers cleanup.
func startDaemon(t *testing.T, p paths, configure func(*daemon.Server)) *daemon.Server {
	t.Helper()
	srv, err := daemon.Listen(daemon.Options{SocketPath: p.socket, PIDPath: p.pid, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv.Handle(api.MethodDaemonStop, func(context.Context, json.RawMessage) (any, error) {
		srv.Stop()
		return api.EmptyData{}, nil
	})
	if configure != nil {
		configure(srv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	return srv
}

func connect(t *testing.T, p paths, opts client.Options) (*client.Client, error) {
	t.Helper()
	opts.SocketPath = p.socket
	if opts.Version == "" {
		opts.Version = "test"
	}
	return client.Connect(context.Background(), opts)
}

func mustConnect(t *testing.T, p paths) *client.Client {
	t.Helper()
	c, err := connect(t, p, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestConnectHandshakesAndStatusWorks(t *testing.T) {
	p := newPaths(t)
	startDaemon(t, p, nil)
	c := mustConnect(t, p)

	status, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "test" || status.PID != os.Getpid() {
		t.Errorf("status = %+v", status)
	}
}

type echo struct {
	Text string `json:"text"`
}

func TestCallDecodesResultsAndSurfacesStructuredErrors(t *testing.T) {
	p := newPaths(t)
	startDaemon(t, p, func(s *daemon.Server) {
		s.Handle(api.MethodTicketGet, func(_ context.Context, params json.RawMessage) (any, error) {
			var in echo
			json.Unmarshal(params, &in)
			return echo{Text: strings.ToUpper(in.Text)}, nil
		})
		s.Handle(api.MethodTicketStop, func(context.Context, json.RawMessage) (any, error) {
			return nil, &api.Error{Code: api.CodeBlocked, Message: "dirty checkout", Details: []string{"api"}}
		})
	})
	c := mustConnect(t, p)

	var got echo
	if err := c.Call(context.Background(), api.MethodTicketGet, echo{Text: "hi"}, &got); err != nil || got.Text != "HI" {
		t.Fatalf("Call = %+v, %v", got, err)
	}

	err := c.Call(context.Background(), api.MethodTicketStop, nil, nil)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeBlocked || len(apiErr.Details) != 1 {
		t.Fatalf("error = %v, want the daemon's blocked error with details", err)
	}
	if err := c.Call(context.Background(), api.MethodStatus, nil, nil); err != nil {
		t.Errorf("the client is unusable after a failed call: %v", err)
	}
}

func TestConnectWithoutADaemonReportsNotRunning(t *testing.T) {
	p := newPaths(t)
	tests := []struct {
		name  string
		setup func()
	}{
		{"no socket at all", func() {}},
		{"a stale socket nobody listens on", func() {
			l, err := net.Listen("unix", p.socket)
			if err != nil {
				t.Fatal(err)
			}
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			l.Close()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Remove(p.socket)
			tt.setup()
			_, err := connect(t, p, client.Options{})
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.CodeNotRunning || !strings.Contains(apiErr.Message, p.socket) {
				t.Fatalf("error = %v, want not_running naming the socket", err)
			}
		})
	}
}

func TestConnectAutoStartsTheDaemonExactlyOnce(t *testing.T) {
	p := newPaths(t)
	var spawns atomic.Int32
	var srv *daemon.Server
	spawner := func(context.Context) error {
		spawns.Add(1)
		srv = startDaemon(t, p, nil)
		return nil
	}

	first, err := connect(t, p, client.Options{Spawner: spawner})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := connect(t, p, client.Options{Spawner: spawner})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if n := spawns.Load(); n != 1 {
		t.Errorf("spawner ran %d times, want 1 (the second connect found the daemon already running)", n)
	}
	if _, err := first.Status(context.Background()); err != nil {
		t.Errorf("status after auto-start: %v", err)
	}
	_ = srv
}

func TestAutoStartWaitsForASlowDaemon(t *testing.T) {
	p := newPaths(t)
	spawner := func(context.Context) error {
		go func() {
			time.Sleep(300 * time.Millisecond)
			startDaemon(t, p, nil)
		}()
		return nil
	}
	c, err := connect(t, p, client.Options{Spawner: spawner, StartupTimeout: 5 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("a daemon that takes a moment to bind should still be found: %v", err)
	}
	c.Close()
}

func TestAutoStartFailures(t *testing.T) {
	t.Run("the spawner fails", func(t *testing.T) {
		boom := errors.New("cannot exec")
		_, err := connect(t, newPaths(t), client.Options{Spawner: func(context.Context) error { return boom }})
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeUnavailable || !strings.Contains(apiErr.Message, "cannot exec") {
			t.Fatalf("error = %v, want unavailable mentioning the cause", err)
		}
	})

	t.Run("the daemon never appears", func(t *testing.T) {
		clock := testkit.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
		var wg sync.WaitGroup
		result := make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := connect(t, newPaths(t), client.Options{
				Spawner:        func(context.Context) error { return nil },
				Clock:          clock,
				StartupTimeout: 5 * time.Second,
				PollInterval:   100 * time.Millisecond,
				StartHint:      "see rigd.log",
			})
			result <- err
		}()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case err := <-result:
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.CodeUnavailable || !strings.Contains(apiErr.Message, "did not start") || !strings.Contains(apiErr.Message, "see rigd.log") {
					t.Fatalf("error = %v, want unavailable saying the daemon did not start, with the hint", err)
				}
				wg.Wait()
				return
			case <-deadline:
				t.Fatal("Connect never gave up")
			default:
				clock.Advance(100 * time.Millisecond)
				time.Sleep(time.Millisecond)
			}
		}
	})

	t.Run("auto-start disabled", func(t *testing.T) {
		_, err := connect(t, newPaths(t), client.Options{})
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeNotRunning {
			t.Fatalf("error = %v, want not_running", err)
		}
	})
}

func TestConnectRejectsAServerSpeakingAnotherProtocol(t *testing.T) {
	p := newPaths(t)
	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		dec, enc := api.NewDecoder(conn), api.NewEncoder(conn)
		req, err := dec.ReadRequest()
		if err != nil {
			return
		}
		resp, _ := api.Success(req.ID, api.HelloResult{Protocol: 2, Server: "future"})
		enc.Write(resp)
	}()
	_, err = connect(t, p, client.Options{})
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("error = %v, want a protocol version complaint", err)
	}
}

func TestStreamDeliversItemsUntilTheEndMarker(t *testing.T) {
	p := newPaths(t)
	startDaemon(t, p, func(s *daemon.Server) {
		s.HandleStream(api.MethodLogsStream, func(_ context.Context, _ json.RawMessage, send func(any) error) error {
			for i := 1; i <= 3; i++ {
				if err := send(map[string]int{"n": i}); err != nil {
					return err
				}
			}
			return nil
		})
		s.HandleStream(api.MethodEventsSubscribe, func(ctx context.Context, _ json.RawMessage, send func(any) error) error {
			send(map[string]string{"first": "item"})
			<-ctx.Done()
			return ctx.Err()
		})
	})

	t.Run("finite stream", func(t *testing.T) {
		c := mustConnect(t, p)
		var n []int
		err := c.Stream(context.Background(), api.MethodLogsStream, nil, func(raw json.RawMessage) error {
			var item struct{ N int }
			json.Unmarshal(raw, &item)
			n = append(n, item.N)
			return nil
		})
		if err != nil || len(n) != 3 || n[2] != 3 {
			t.Fatalf("items = %v, err = %v", n, err)
		}
	})

	t.Run("cancelling the context ends an open stream", func(t *testing.T) {
		c := mustConnect(t, p)
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan struct{}, 1)
		result := make(chan error, 1)
		go func() {
			result <- c.Stream(ctx, api.MethodEventsSubscribe, nil, func(json.RawMessage) error {
				got <- struct{}{}
				return nil
			})
		}()
		<-got
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Stream did not return after cancellation")
		}
	})

	t.Run("a callback error stops the stream", func(t *testing.T) {
		c := mustConnect(t, p)
		stop := errors.New("enough")
		err := c.Stream(context.Background(), api.MethodLogsStream, nil, func(json.RawMessage) error { return stop })
		if !errors.Is(err, stop) {
			t.Errorf("error = %v, want the callback's error", err)
		}
	})
}

func TestStopShutsTheDaemonDownAndWaitsForTheSocketToGo(t *testing.T) {
	p := newPaths(t)
	startDaemon(t, p, nil)
	c := mustConnect(t, p)

	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after Stop returned: %v", err)
	}
	if _, err := connect(t, p, client.Options{}); err == nil {
		t.Error("the daemon still accepts connections after Stop")
	}
}

func TestACallOnALostConnectionFailsClearly(t *testing.T) {
	p := newPaths(t)
	srv := startDaemon(t, p, nil)
	c := mustConnect(t, p)
	srv.Stop()
	time.Sleep(200 * time.Millisecond)
	err := c.Call(context.Background(), api.MethodStatus, nil, nil)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeNotRunning {
		t.Fatalf("error = %v, want not_running", err)
	}
}

func TestACallHonorsTheContextDeadline(t *testing.T) {
	p := newPaths(t)
	startDaemon(t, p, func(s *daemon.Server) {
		s.Handle(api.MethodTicketGet, func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})
	c := mustConnect(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := c.Call(ctx, api.MethodTicketGet, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the context deadline", err)
	}
}
