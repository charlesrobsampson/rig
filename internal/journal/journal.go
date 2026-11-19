// Package journal keeps a ticket's journal: records agents leave about their work
// (feedback on rig or their environment, results, anything an orchestrator wants
// kept). An agent writes a record as one line of JSON; rig validates it, stamps who
// and when itself, and appends it to journal.jsonl in the ticket directory, which no
// agent can reach (docs/formats.md §14).
package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/fsutil"
)

const (
	FileName    = "journal.jsonl"
	offsetsName = "journal.offsets.json"
	// MaxLineBytes bounds one record as written by an agent.
	MaxLineBytes = 64 << 10
	MaxTextBytes = 16 << 10
)

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// Entry is a record as an agent writes it. Anything else in the line (a ticket, a
// time) is ignored: rig knows those better than the agent does.
type Entry struct {
	Kind string          `json:"kind"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// IsRecordLine tells a record from the state words and cost lines that share the
// events file. A line that opens like an object is a record even when it is broken,
// so it is never read as a state.
func IsRecordLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "{")
}

func Parse(line string) (Entry, error) {
	if len(line) > MaxLineBytes {
		return Entry{}, fmt.Errorf("record is %d bytes, over the %d limit", len(line), MaxLineBytes)
	}
	if !utf8.ValidString(line) {
		return Entry{}, errors.New("record is not valid UTF-8")
	}
	var e Entry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return Entry{}, fmt.Errorf("record is not a JSON object with a kind: %w", err)
	}
	return e, e.Validate()
}

// Validate checks an entry and drops a null data field.
func (e *Entry) Validate() error {
	if !kindPattern.MatchString(e.Kind) {
		return fmt.Errorf("kind %q must be lowercase letters, digits, '.', '_' or '-', starting with a letter, at most 64 long", e.Kind)
	}
	if len(e.Text) > MaxTextBytes {
		return fmt.Errorf("text is %d bytes, over the %d limit", len(e.Text), MaxTextBytes)
	}
	if !utf8.ValidString(e.Text) {
		return errors.New("text is not valid UTF-8")
	}
	data := bytes.TrimSpace(e.Data)
	switch {
	case len(data) == 0 || string(data) == "null":
		e.Data = nil
	case data[0] != '{' || !json.Valid(data):
		return errors.New("data must be a JSON object")
	default:
		e.Data = data
	}
	return nil
}

// Append adds one record to the ticket's journal.
func Append(ticketDir string, r api.JournalRecord) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode journal record: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(ticketDir, FileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	// One write per line: appends from several writers never interleave inside a line.
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("append to journal: %w", err)
	}
	return f.Close()
}

// Read returns a ticket's records in the order they were written. A damaged line is
// skipped rather than hiding the records after it.
func Read(ticketDir string) ([]api.JournalRecord, error) {
	f, err := os.Open(filepath.Join(ticketDir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()
	var out []api.JournalRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 4*MaxLineBytes)
	for scanner.Scan() {
		var r api.JournalRecord
		if json.Unmarshal(scanner.Bytes(), &r) == nil && r.Kind != "" {
			out = append(out, r)
		}
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("read journal: %w", err)
	}
	return out, nil
}

// Filter keeps the records matching p, oldest first, and of those the newest p.Limit.
func Filter(records []api.JournalRecord, p api.JournalListParams) []api.JournalRecord {
	var out []api.JournalRecord
	for _, r := range records {
		if (p.Ticket == "" || r.Ticket == p.Ticket) && (p.Service == "" || r.Service == p.Service) && (p.Kind == "" || r.Kind == p.Kind) {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b api.JournalRecord) int { return a.At.Compare(b.At) })
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[len(out)-p.Limit:]
	}
	return out
}

// Offset is how far into a service's events file has been journaled. It is kept on
// disk so a restarted daemon neither loses nor repeats records; a missing or damaged
// file means the start.
func Offset(ticketDir, service string) int64 {
	return readOffsets(ticketDir)[service]
}

func SetOffset(ticketDir, service string, offset int64) error {
	offsets := readOffsets(ticketDir)
	offsets[service] = offset
	data, err := json.Marshal(offsets)
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(filepath.Join(ticketDir, offsetsName), data)
}

func readOffsets(ticketDir string) map[string]int64 {
	offsets := map[string]int64{}
	data, err := os.ReadFile(filepath.Join(ticketDir, offsetsName))
	if err != nil || json.Unmarshal(data, &offsets) != nil {
		return map[string]int64{}
	}
	return offsets
}
