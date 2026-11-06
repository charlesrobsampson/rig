package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/charlesrobsampson/rig/internal/api"
)

// serveConn reads requests until the client leaves. Each request runs in its own
// goroutine so a long stream does not block the connection; responses carry the
// request id, so clients match them up.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer s.connWG.Done()
	defer func() {
		s.connMu.Lock()
		delete(s.conns, conn)
		s.connMu.Unlock()
		conn.Close()
	}()
	ctx, cancel := context.WithCancel(ctx)
	var requests sync.WaitGroup
	defer requests.Wait()
	defer cancel() // runs before requests.Wait, so streams end when the client leaves

	enc, dec := api.NewEncoder(conn), api.NewDecoder(conn)
	var greeted atomic.Bool
	for {
		req, err := dec.ReadRequest()
		switch {
		case err == nil:
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
			return
		case errors.Is(err, api.ErrLineTooLong):
			enc.Write(api.Failure("", &api.Error{Code: api.CodeInvalid, Message: "message exceeds the 1 MiB limit"}))
			continue
		default:
			enc.Write(api.Failure("", &api.Error{Code: api.CodeInvalid, Message: err.Error()}))
			continue
		}
		s.inflight.Add(1)
		requests.Add(1)
		go func() {
			defer s.inflight.Done()
			defer requests.Done()
			s.dispatch(ctx, enc, req, &greeted)
		}()
	}
}

func (s *Server) dispatch(ctx context.Context, enc *api.Encoder, req api.Request, greeted *atomic.Bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("handler panic", "method", req.Method, "panic", r, "stack", string(debug.Stack()))
			enc.Write(api.Failure(req.ID, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("handler panic: %v", r)}))
		}
	}()

	if verr := req.Validate(); verr != nil {
		enc.Write(api.Failure(req.ID, verr))
		return
	}
	if req.Method == api.MethodHello {
		greeted.Store(true)
		reply(enc, req.ID, api.HelloResult{Protocol: api.ProtocolVersion, Server: "rigd " + s.opts.Version}, nil)
		return
	}
	if !greeted.Load() {
		enc.Write(api.Failure(req.ID, &api.Error{Code: api.CodeInvalid, Message: "send hello first"}))
		return
	}

	s.mu.RLock()
	handler, isUnary := s.handlers[req.Method]
	stream, isStream := s.streams[req.Method]
	s.mu.RUnlock()
	switch {
	case isUnary:
		result, err := handler(ctx, req.Params)
		reply(enc, req.ID, result, err)
	case isStream:
		err := stream(ctx, req.Params, func(item any) error {
			msg, err := api.StreamItem(req.ID, item)
			if err != nil {
				return err
			}
			return enc.Write(msg)
		})
		if err != nil {
			reply(enc, req.ID, nil, err)
			return
		}
		enc.Write(api.EndOfStream(req.ID))
	default:
		enc.Write(api.Failure(req.ID, &api.Error{Code: api.CodeInvalid, Message: fmt.Sprintf("method %q is not implemented by this daemon", req.Method)}))
	}
}

func reply(enc *api.Encoder, id string, result any, err error) {
	if err != nil {
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			apiErr = &api.Error{Code: api.CodeInternal, Message: err.Error()}
		}
		enc.Write(api.Failure(id, apiErr))
		return
	}
	resp, encErr := api.Success(id, result)
	if encErr != nil {
		enc.Write(api.Failure(id, &api.Error{Code: api.CodeInternal, Message: encErr.Error()}))
		return
	}
	enc.Write(resp)
}
