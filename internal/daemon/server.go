// Package daemon is rigd's server: a unix socket speaking the api protocol, with a
// single-instance lock, a handler registry, and graceful shutdown.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
)

// maxSocketPath is below the 104-byte limit of macOS and the 108 of Linux.
const maxSocketPath = 103

const defaultShutdownGrace = 3 * time.Second

type Options struct {
	SocketPath string
	PIDPath    string
	Version    string
	Clock      clock.Clock  // nil means the real clock
	Logger     *slog.Logger // nil discards logs
	// ShutdownGrace is how long in-flight requests get to finish on shutdown.
	ShutdownGrace time.Duration
}

// Handler answers one request. Return an *api.Error to control the error code and
// details; any other error becomes an internal error.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// StreamHandler sends items until it returns; ctx is cancelled when the client
// disconnects or the daemon stops.
type StreamHandler func(ctx context.Context, params json.RawMessage, send func(item any) error) error

type Server struct {
	opts    Options
	log     *slog.Logger
	clock   clock.Clock
	started time.Time
	lock    *os.File
	ln      net.Listener

	mu       sync.RWMutex
	handlers map[string]Handler
	streams  map[string]StreamHandler

	connMu sync.Mutex
	conns  map[net.Conn]struct{}

	inflight sync.WaitGroup // requests being handled
	connWG   sync.WaitGroup // connection goroutines

	stop     chan struct{}
	stopOnce sync.Once
}

// Listen takes the single-instance lock and binds the socket. It fails if another
// daemon is running; a stale pid file or socket left by a crash is cleaned up.
func Listen(opts Options) (*Server, error) {
	if len(opts.SocketPath) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is too long (%d bytes, limit %d); use a shorter rig home", opts.SocketPath, len(opts.SocketPath), maxSocketPath)
	}
	for _, dir := range []string{filepath.Dir(opts.SocketPath), filepath.Dir(opts.PIDPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	lock, err := acquireInstance(opts.PIDPath)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Server, error) {
		releaseInstance(lock, opts.PIDPath)
		return nil, err
	}
	if err := clearStaleSocket(opts.SocketPath); err != nil {
		return fail(err)
	}
	ln, err := net.Listen("unix", opts.SocketPath)
	if err != nil {
		return fail(fmt.Errorf("listen on %s: %w", opts.SocketPath, err))
	}
	if err := os.Chmod(opts.SocketPath, 0o600); err != nil {
		ln.Close()
		return fail(err)
	}

	s := &Server{
		opts:     opts,
		log:      opts.Logger,
		clock:    opts.Clock,
		lock:     lock,
		ln:       ln,
		handlers: map[string]Handler{},
		streams:  map[string]StreamHandler{},
		conns:    map[net.Conn]struct{}{},
		stop:     make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.clock == nil {
		s.clock = clock.Real()
	}
	if s.opts.ShutdownGrace == 0 {
		s.opts.ShutdownGrace = defaultShutdownGrace
	}
	s.started = s.clock.Now()
	s.Handle(api.MethodStatus, s.status)
	return s, nil
}

func (s *Server) SocketPath() string { return s.opts.SocketPath }

// Handle registers a unary method. Registering an unknown or streaming method is a
// programming error and panics.
func (s *Server) Handle(method string, h Handler) {
	if !api.KnownMethod(method) || api.IsStreaming(method) {
		panic(fmt.Sprintf("daemon: Handle(%q): not a documented unary method", method))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

func (s *Server) HandleStream(method string, h StreamHandler) {
	if !api.IsStreaming(method) {
		panic(fmt.Sprintf("daemon: HandleStream(%q): not a streaming method", method))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[method] = h
}

// Stop asks Serve to shut down, as cancelling its context does.
func (s *Server) Stop() { s.stopOnce.Do(func() { close(s.stop) }) }

// Serve accepts connections until ctx is cancelled or Stop is called, then shuts down
// gracefully: in-flight requests get ShutdownGrace to finish, connections are closed,
// and the socket and pid file are removed.
func (s *Server) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
		s.ln.Close()
	}()
	defer s.shutdown()

	s.log.Info("listening", "socket", s.opts.SocketPath, "pid", os.Getpid())
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.connMu.Lock()
		s.conns[conn] = struct{}{}
		s.connMu.Unlock()
		s.connWG.Add(1)
		go s.serveConn(ctx, conn)
	}
}

func (s *Server) shutdown() {
	s.log.Info("shutting down")
	grace := make(chan struct{})
	go func() { s.inflight.Wait(); close(grace) }()
	select {
	case <-grace:
	case <-time.After(s.opts.ShutdownGrace):
		s.log.Warn("requests still running after the grace period; closing connections anyway")
	}
	s.connMu.Lock()
	for conn := range s.conns {
		conn.Close()
	}
	s.connMu.Unlock()
	s.connWG.Wait()

	s.ln.Close()
	os.Remove(s.opts.SocketPath)
	releaseInstance(s.lock, s.opts.PIDPath)
}

func (s *Server) status(context.Context, json.RawMessage) (any, error) {
	return api.StatusResult{
		Version:       s.opts.Version,
		PID:           os.Getpid(),
		UptimeSeconds: int64(s.clock.Now().Sub(s.started) / time.Second),
	}, nil
}
