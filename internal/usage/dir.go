package usage

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/pricing"
)

// Session is what one transcript file adds up to, priced.
type Session struct {
	// File is the transcript's path under the directory, slash separated.
	File       string
	Transcript Transcript
	CostUSD    float64
	// Unpriced are the models seen that the price table does not list: their tokens
	// are counted, their cost is not in CostUSD.
	Unpriced []string
}

// Dir is what every transcript under one directory adds up to.
type Dir struct {
	Path     string
	Total    Transcript
	CostUSD  float64
	Unpriced []string
	Sessions []Session
}

// ErrNotTranscripts says a directory has no Claude Code transcripts layout.
var ErrNotTranscripts = errors.New("not a Claude Code transcripts folder (expected projects/<folder>/*.jsonl)")

// ReadDir reads every transcript under dir/projects (sessions and sub-agent files),
// counting each request once across all of them, and prices each request with prices.
// A zero since counts everything; otherwise only entries at or after it count, so a
// request stamped before it is left out, and so is the time before it. Sessions with
// nothing counted are left out. A directory with no transcript files at all is an
// error naming it, wrapping ErrNotTranscripts.
func ReadDir(dir string, since time.Time, prices *pricing.Table) (Dir, error) {
	d := Dir{Path: dir}
	var files []string
	root := filepath.Join(dir, "projects")
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case e.Type()&fs.ModeSymlink != 0, e.IsDir(), !strings.HasSuffix(path, ".jsonl"):
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return d, fmt.Errorf("%s: %w", dir, err)
	}
	if len(files) == 0 {
		return d, fmt.Errorf("%s: %w", dir, ErrNotTranscripts)
	}
	// a session before its sub-agents, so a request both repeat belongs to the session
	sort.Slice(files, func(i, j int) bool {
		return strings.TrimSuffix(files[i], ".jsonl") < strings.TrimSuffix(files[j], ".jsonl")
	})
	seen := map[string]bool{}
	var allStamps []time.Time
	allModels, allUnpriced := map[string]bool{}, map[string]bool{}
	for _, path := range files {
		var s Session
		var stamps []time.Time
		models, unpriced := map[string]bool{}, map[string]bool{}
		err := scanFile(path, func(l line) {
			if !since.IsZero() && l.Timestamp.Before(since) {
				return // a line with no timestamp is before any time we were asked about
			}
			if !l.Timestamp.IsZero() {
				stamps = append(stamps, l.Timestamp)
			}
			u := l.Message.Usage
			key := l.Message.ID + "/" + l.RequestID
			if l.Type != "assistant" || u == nil || seen[key] {
				return
			}
			seen[key] = true
			s.Transcript.Requests++
			s.Transcript.Tokens.Add(Tokens{Input: u.Input, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Output: u.Output})
			w5, w1 := u.CacheCreation.FiveMinute, u.CacheCreation.OneHour
			if rest := u.CacheWrite - w5 - w1; rest > 0 {
				w5 += rest // no split, or a split short of the total: the 5 minute rate
			}
			usd, ok := prices.Cost(l.Message.Model, pricing.Request{Input: u.Input, CacheWrite5m: w5, CacheWrite1h: w1, CacheRead: u.CacheRead, Output: u.Output})
			s.CostUSD += usd
			if l.Message.Model != "" {
				models[l.Message.Model] = true
			}
			if !ok {
				unpriced[l.Message.Model] = true
			}
		})
		if err != nil {
			return d, fmt.Errorf("%s: %w", path, err)
		}
		if s.Transcript.Requests == 0 && len(stamps) == 0 {
			continue
		}
		rel, _ := filepath.Rel(dir, path)
		s.File = filepath.ToSlash(rel)
		s.Transcript.finish(stamps, models)
		s.Unpriced = sortedKeys(unpriced)
		d.Sessions = append(d.Sessions, s)
		d.Total.Requests += s.Transcript.Requests
		d.Total.Tokens.Add(s.Transcript.Tokens)
		d.CostUSD += s.CostUSD
		allStamps = append(allStamps, stamps...)
		for m := range models {
			allModels[m] = true
		}
		for m := range unpriced {
			allUnpriced[m] = true
		}
	}
	d.Total.finish(allStamps, allModels)
	d.Unpriced = sortedKeys(allUnpriced)
	return d, nil
}

// finish sets the models, first and last time, and working time from what was collected.
func (t *Transcript) finish(stamps []time.Time, models map[string]bool) {
	t.Models = sortedKeys(models)
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	for i, s := range stamps {
		if i == 0 {
			t.First = s
		}
		t.Last = s
		if i > 0 {
			if gap := s.Sub(stamps[i-1]); gap <= WorkGap {
				t.WorkSeconds += int64(gap.Seconds())
			}
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
