// Package usage reads what an agent's model consumed from the agent's own Claude Code
// transcripts: rig gives every container agent (and a host agent with a credential
// profile) its own configuration directory, and Claude Code writes each request's token
// counts and model there. Cost comes from the harness's own report; tokens and models
// come from here.
package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Tokens counts what a model read and wrote. Reading from the prompt cache is much
// cheaper and faster than fresh input, which is why the kinds are kept apart.
type Tokens struct {
	Input      int64 `json:"input"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
	Output     int64 `json:"output"`
}

// In is everything the model read: fresh input, cache writes and cache reads.
func (t Tokens) In() int64 { return t.Input + t.CacheWrite + t.CacheRead }

// Total is everything read and written.
func (t Tokens) Total() int64 { return t.In() + t.Output }

// Add adds o to t.
func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.CacheWrite += o.CacheWrite
	t.CacheRead += o.CacheRead
	t.Output += o.Output
}

// Transcript is what one agent's transcripts add up to.
type Transcript struct {
	Tokens   Tokens
	Requests int
	// Models are the models that answered, sorted.
	Models      []string
	First, Last time.Time
	// WorkSeconds is the time the agent spent working: the gaps between consecutive
	// transcript entries (all files merged) that are at most WorkGap.
	WorkSeconds int64
}

// WorkGap is the longest silence between two transcript entries that still counts as
// the agent working. An agent waiting on a long tool call (rig's own gate runs the race
// detector and the docker tests, 5 to 10 minutes) writes nothing between the call and
// its result; waiting on a person or a gate is usually far longer.
const WorkGap = 15 * time.Minute

// Signature is a cheap stand-in for the transcripts' content: file count, total size
// and newest modification, read from stat alone, so a caller can tell that nothing
// changed since it last read them.
func Signature(configDir string) string {
	var n int
	var size int64
	var newest time.Time
	filepath.WalkDir(filepath.Join(configDir, "projects"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n++
			size += info.Size()
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return nil
	})
	return fmt.Sprintf("%d/%d/%d", n, size, newest.UnixNano())
}

type line struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			Output     int64 `json:"output_tokens"`
			// CacheCreation splits the cache writes by time to live, when Claude Code says
			CacheCreation struct {
				FiveMinute int64 `json:"ephemeral_5m_input_tokens"`
				OneHour    int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// ReadTranscripts adds up every transcript under configDir/projects (sessions and their
// subagents). Claude Code repeats a request's usage on each line it writes for it, so a
// request counts once. A missing directory is an agent that never ran: nothing, no error.
func ReadTranscripts(configDir string) (Transcript, error) {
	var t Transcript
	var stamps []time.Time
	seen := map[string]bool{}
	models := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(configDir, "projects"), func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return filepath.SkipAll
		case err != nil:
			return err
		case d.Type()&fs.ModeSymlink != 0, d.IsDir(), !strings.HasSuffix(path, ".jsonl"):
			return nil
		}
		return readFile(path, &t, &stamps, seen, models)
	})
	for m := range models {
		t.Models = append(t.Models, m)
	}
	sort.Strings(t.Models)
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap <= WorkGap {
			t.WorkSeconds += int64(gap.Seconds())
		}
	}
	return t, err
}

func readFile(path string, t *Transcript, stamps *[]time.Time, seen, models map[string]bool) error {
	return scanFile(path, func(l line) {
		if !l.Timestamp.IsZero() {
			*stamps = append(*stamps, l.Timestamp)
			if t.First.IsZero() || l.Timestamp.Before(t.First) {
				t.First = l.Timestamp
			}
			if l.Timestamp.After(t.Last) {
				t.Last = l.Timestamp
			}
		}
		u := l.Message.Usage
		key := l.Message.ID + "/" + l.RequestID
		if l.Type != "assistant" || u == nil || seen[key] {
			return
		}
		seen[key] = true
		t.Requests++
		t.Tokens.Add(Tokens{Input: u.Input, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Output: u.Output})
		if l.Message.Model != "" {
			models[l.Message.Model] = true
		}
	})
}

// scanFile calls visit with each line of a transcript that parses.
func scanFile(path string, visit func(line)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // a line can carry a whole file the agent read
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		visit(l)
	}
	return sc.Err()
}
