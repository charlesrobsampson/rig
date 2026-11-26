// Package timeline keeps a ticket's timeline: one append-only file, timeline.jsonl,
// in which the daemon records every change once, in order (docs/formats.md §15).
// Entries are numbered from 1 per ticket without gaps, so a reader can ask for
// everything after a seq. One Log serializes the appends of the daemon; a second
// writer on the same ticket directory is not supported.
package timeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
)

const FileName = "timeline.jsonl"

// Kinds of entry.
const (
	TicketStarted      = "ticket.started"
	TicketStopped      = "ticket.stopped"
	PhaseMoved         = "phase.moved"
	GateApproved       = "gate.approved"
	PhaseRequested     = "phase.requested"
	JournalRecorded    = "journal.recorded"
	MessageSent        = "message.sent"
	ReviewRoundStarted = "review.round.started"
	ReviewRoundClosed  = "review.round.closed"
	ReviewDecided      = "review.decided"
	ReviewLoopEnded    = "review.loop.ended"
	SyncDone           = "sync.done"
	PushDone           = "push.done"
)

// Entry is one line of timeline.jsonl.
type Entry struct {
	Seq     int             `json:"seq"`
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	By      string          `json:"by,omitempty"`
	Service string          `json:"service,omitempty"`
	Data    json.RawMessage `json:"data"`
}

type PhaseMovedData struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}
type GateApprovedData struct {
	Gate string `json:"gate"`
}
type PhaseRequestedData struct {
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}
type JournalRecordedData struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}
type MessageSentData struct {
	Service   string `json:"service"`
	From      string `json:"from"`
	Delivered bool   `json:"delivered"`
}
type ReviewRoundStartedData struct {
	Round  int    `json:"round"`
	Commit string `json:"commit"`
}
type ReviewRoundClosedData struct {
	Round     int    `json:"round"`
	Verdict   string `json:"verdict"`
	Blocking  int    `json:"blocking"`
	ShouldFix int    `json:"should_fix"`
	Nit       int    `json:"nit"`
}
type ReviewDecidedData struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}
type ReviewLoopEndedData struct {
	Rounds int    `json:"rounds"`
	Status string `json:"status"`
}
type SyncDoneData struct {
	Service   string   `json:"service"`
	Behind    int      `json:"behind"`
	Merged    string   `json:"merged,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}
type PushDoneData struct {
	Service string `json:"service"`
	Mode    string `json:"mode"`
	PRURL   string `json:"pr_url,omitempty"`
	Files   int    `json:"files"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Error   string `json:"error,omitempty"`
}

// Data encodes a typed payload for an Entry.
func Data(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// ExcerptBytes bounds the journal text a timeline entry carries; the full text is
// in journal.jsonl.
const ExcerptBytes = 200

// Excerpt shortens text to ExcerptBytes on a character boundary, marking a cut.
func Excerpt(s string) string {
	if len(s) <= ExcerptBytes {
		return s
	}
	cut := ExcerptBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Log appends entries. A nil *Log appends nothing, so callers need no check.
type Log struct {
	now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	last  map[string]int
}

func New(now func() time.Time) *Log {
	if now == nil {
		now = time.Now
	}
	return &Log{now: now, locks: map[string]*sync.Mutex{}, last: map[string]int{}}
}

// Append writes e as the ticket's next entry (Seq and At are set here) and returns it.
func (l *Log) Append(ticketDir string, e Entry) (Entry, error) {
	if l == nil {
		return Entry{}, nil
	}
	l.mu.Lock()
	lock := l.locks[ticketDir]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[ticketDir] = lock
	}
	l.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	path := filepath.Join(ticketDir, FileName)
	last, known := l.lastSeq(ticketDir)
	if !known {
		var err error
		if last, err = lastInFile(path); err != nil {
			return Entry{}, err
		}
	}
	e.Seq, e.At = last+1, l.now().UTC()
	if len(e.Data) == 0 {
		e.Data = json.RawMessage(`{}`)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return Entry{}, fmt.Errorf("encode timeline entry: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Entry{}, fmt.Errorf("open timeline: %w", err)
	}
	// One write per line, so a crash leaves a whole line or a torn last one.
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return Entry{}, fmt.Errorf("append to timeline: %w", err)
	}
	if err := f.Close(); err != nil {
		return Entry{}, err
	}
	l.setLast(ticketDir, e.Seq)
	return e, nil
}

func (l *Log) lastSeq(dir string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.last[dir]
	return n, ok
}

func (l *Log) setLast(dir string, n int) {
	l.mu.Lock()
	l.last[dir] = n
	l.mu.Unlock()
}

// lastInFile finds the last seq in the file, first cutting a torn last line (a crash
// in the middle of a write) so the next entry starts on a line of its own.
func lastInFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read timeline: %w", err)
	}
	if n := bytes.LastIndexByte(data, '\n'); n+1 < len(data) {
		data = data[:n+1]
		if err := os.Truncate(path, int64(len(data))); err != nil {
			return 0, fmt.Errorf("repair timeline: %w", err)
		}
	}
	last := 0
	for _, e := range parse(data) {
		last = e.Seq
	}
	return last, nil
}

func parse(data []byte) []Entry {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Seq > 0 && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out
}

// Read returns the entries after since (0 for all), in order. A ticket with no
// timeline reads as empty; a damaged line is skipped.
func Read(ticketDir string, since int) ([]Entry, error) {
	data, err := os.ReadFile(filepath.Join(ticketDir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read timeline: %w", err)
	}
	var out []Entry
	for _, e := range parse(data) {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out, nil
}
