package usage_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/usage"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A request's usage is repeated on every line Claude Code writes for it (one per content
// block); it must count once.
const session = `{"type":"user","timestamp":"2026-11-25T14:12:50Z","message":{"role":"user","content":"go"}}
{"type":"assistant","timestamp":"2026-11-25T14:12:55Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":1000,"cache_read_input_tokens":5000,"output_tokens":200}}}
{"type":"assistant","timestamp":"2026-11-25T14:12:56Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":1000,"cache_read_input_tokens":5000,"output_tokens":200}}}
not json at all
{"type":"assistant","timestamp":"2026-11-25T14:20:00Z","requestId":"r2","message":{"id":"m2","model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":6000,"output_tokens":50}}}
`

func TestTranscriptsAreSummedCountingEachRequestOnce(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "-work", "s1.jsonl"), session)
	write(t, filepath.Join(dir, "projects", "-work", "s1", "subagents", "a.jsonl"), `{"type":"assistant","timestamp":"2026-11-25T14:30:00Z","requestId":"r3","message":{"id":"m3","model":"claude-haiku-5-5","usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":9}}}`+"\n")
	write(t, filepath.Join(dir, "history.jsonl"), `{"type":"assistant","requestId":"x","message":{"id":"x","usage":{"output_tokens":999999}}}`+"\n")

	got, err := usage.ReadTranscripts(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := usage.Tokens{Input: 16, CacheWrite: 1000, CacheRead: 11000, Output: 259}
	if got.Tokens != want || got.Requests != 3 {
		t.Errorf("tokens = %+v (%d requests), want %+v (3)", got.Tokens, got.Requests, want)
	}
	if len(got.Models) != 3 || got.Models[0] != "claude-haiku-5-5" || got.Models[2] != "claude-sonnet-5-5" {
		t.Errorf("models = %v", got.Models)
	}
	if !got.First.Equal(time.Date(2026, 11, 25, 14, 12, 50, 0, time.UTC)) || !got.Last.Equal(time.Date(2026, 11, 25, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("first %v, last %v", got.First, got.Last)
	}
	if got.Tokens.In() != 12016 || got.Tokens.Total() != 12275 {
		t.Errorf("in %d, total %d", got.Tokens.In(), got.Tokens.Total())
	}
}

func TestNoTranscriptsIsNotAnError(t *testing.T) {
	got, err := usage.ReadTranscripts(filepath.Join(t.TempDir(), "none"))
	if err != nil || got.Requests != 0 || got.Tokens != (usage.Tokens{}) {
		t.Errorf("%+v, %v", got, err)
	}
}

func TestWorkSecondsCountsGapsUpToTheLimitAcrossFilesMerged(t *testing.T) {
	line := func(ts string) string { return `{"type":"user","timestamp":"` + ts + `"}` + "\n" }
	tests := []struct {
		name  string
		files []string
		want  int64
	}{
		{"one entry", []string{line("2026-11-25T10:00:00Z")}, 0},
		{"small gaps add up, unsorted", []string{line("2026-11-25T10:02:00Z") + line("2026-11-25T10:00:00Z") + line("2026-11-25T10:00:30Z")}, 120},
		{"a gap of exactly the limit counts, a longer one does not", []string{line("2026-11-25T10:00:00Z") + line("2026-11-25T10:15:00Z") + line("2026-11-25T10:30:01Z")}, 900},
		{"files are merged before gaps are taken", []string{line("2026-11-25T10:00:00Z") + line("2026-11-25T10:10:00Z"), line("2026-11-25T10:05:00Z")}, 600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for i, f := range tt.files {
				write(t, filepath.Join(dir, "projects", "-work", string(rune('a'+i))+".jsonl"), f)
			}
			got, err := usage.ReadTranscripts(dir)
			if err != nil || got.WorkSeconds != tt.want {
				t.Errorf("WorkSeconds = %d, %v; want %d", got.WorkSeconds, err, tt.want)
			}
		})
	}
}

func TestSignatureChangesWithTheTranscripts(t *testing.T) {
	dir := t.TempDir()
	empty := usage.Signature(dir)
	write(t, filepath.Join(dir, "projects", "-work", "a.jsonl"), "{}\n")
	one := usage.Signature(dir)
	write(t, filepath.Join(dir, "projects", "-work", "a.jsonl"), "{}\n{}\n")
	if two := usage.Signature(dir); empty == one || one == two || usage.Signature(dir) != two {
		t.Errorf("signatures %q %q %q", empty, one, two)
	}
}
