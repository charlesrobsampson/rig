package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxLineBytes is the longest message either side accepts, newline excluded.
const MaxLineBytes = 1 << 20

var ErrLineTooLong = errors.New("message exceeds the 1 MiB line limit")

// Encoder writes one JSON message per line. It is safe for concurrent use, since a
// connection may carry responses and an event stream at once.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Write refuses messages the peer would reject, so an oversized one fails here
// instead of breaking the connection.
func (e *Encoder) Write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	if len(raw) > MaxLineBytes {
		return fmt.Errorf("%w (%d bytes)", ErrLineTooLong, len(raw))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.w.Write(append(raw, '\n'))
	return err
}

type Decoder struct{ r *bufio.Reader }

func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: bufio.NewReaderSize(r, 64<<10)} }

func (d *Decoder) ReadRequest() (Request, error) {
	var req Request
	if err := d.read(&req); err != nil {
		return Request{}, err
	}
	return req, nil
}

func (d *Decoder) ReadResponse() (Response, error) {
	var resp Response
	if err := d.read(&resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

func (d *Decoder) read(into any) error {
	line, err := d.readLine()
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, into); err != nil {
		return fmt.Errorf("decode message: %w", err)
	}
	return nil
}

// readLine returns the next non-empty line without its newline. A line over the limit
// is skipped to its end and reported as ErrLineTooLong, so the next read still works.
func (d *Decoder) readLine() ([]byte, error) {
	for {
		var line []byte
		tooLong := false
		for {
			chunk, err := d.r.ReadSlice('\n')
			if !tooLong {
				line = append(line, chunk...)
				if len(line) > MaxLineBytes+1 {
					tooLong, line = true, nil
				}
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err == nil {
				break
			}
			switch {
			case tooLong:
				return nil, ErrLineTooLong
			case errors.Is(err, io.EOF) && len(line) > 0:
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if tooLong {
			return nil, ErrLineTooLong
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			return line, nil
		}
	}
}
