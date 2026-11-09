package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/charlesrobsampson/rig/internal/terminal"
)

// apiError is an error reply from the Herdr server.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message) }

// call sends one request on a fresh connection (a local socket round trip costs well
// under a millisecond) and decodes the result. Herdr's error replies are mapped to the
// terminal package's sentinels.
func (b *Backend) call(ctx context.Context, socket, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w: herdr session %q is not running (nothing is listening on %s)", terminal.ErrUnavailable, b.opts.Session, socket)
		}
		return fmt.Errorf("%w: connect to herdr: %v", terminal.ErrUnavailable, err)
	}
	defer conn.Close()

	// Cancelling the context interrupts a read that is waiting for the reply.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.SetDeadline(time.Unix(1, 0))
		case <-stop:
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	id := "rig-" + strconv.FormatInt(b.seq.Add(1), 10)
	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return b.ioError(ctx, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return b.ioError(ctx, err)
	}

	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *apiError       `json:"error"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return fmt.Errorf("herdr sent an unreadable reply to %s: %w", method, err)
	}
	if reply.Error != nil {
		return mapError(reply.Error)
	}
	if result != nil {
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("decode herdr %s result: %w", method, err)
		}
	}
	return nil
}

func (b *Backend) ioError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("%w: herdr connection failed: %v", terminal.ErrUnavailable, err)
}

func mapError(e *apiError) error {
	switch e.Code {
	case "pane_not_found", "tab_not_found", "workspace_not_found":
		return fmt.Errorf("%w: %s", terminal.ErrNotFound, e.Message)
	case "server_not_running":
		return fmt.Errorf("%w: %s", terminal.ErrUnavailable, e.Message)
	}
	return e
}
