package usage_test

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/pricing"
	"github.com/charlesrobsampson/rig/internal/usage"
)

func fixture(t *testing.T) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "-work", "s1.jsonl"), session)
	write(t, filepath.Join(dir, "projects", "-work", "s1", "subagents", "a.jsonl"),
		`{"type":"assistant","timestamp":"2026-11-25T14:30:00Z","requestId":"r3","message":{"id":"m3","model":"claude-haiku-5-5","usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":9}}}`+"\n"+
			// the same request as in s1: counted once across files
			`{"type":"assistant","timestamp":"2026-11-25T14:30:01Z","requestId":"r2","message":{"id":"m2","model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":6000,"output_tokens":50}}}`+"\n")
	write(t, filepath.Join(dir, "projects", "-other", "s2.jsonl"),
		`{"type":"assistant","timestamp":"2026-12-02T09:00:00Z","requestId":"r9","message":{"id":"m9","model":"claude-mystery-9","usage":{"input_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}}`+"\n")
	write(t, filepath.Join(dir, "history.jsonl"), `{"type":"assistant","requestId":"x","message":{"id":"x","usage":{"output_tokens":999999}}}`+"\n")
	return dir
}

func table(t *testing.T) *pricing.Table {
	tab, err := pricing.Default()
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func TestReadDirMatchesTheTranscriptReaderRigStatsUses(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "-work", "s1.jsonl"), session)
	write(t, filepath.Join(dir, "projects", "-work", "s1", "subagents", "a.jsonl"),
		`{"type":"assistant","timestamp":"2026-11-25T14:30:00Z","requestId":"r3","message":{"id":"m3","model":"claude-haiku-5-5","usage":{"input_tokens":1,"output_tokens":9}}}`+"\n")
	want, err := usage.ReadTranscripts(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := usage.ReadDir(dir, time.Time{}, table(t))
	if err != nil {
		t.Fatal(err)
	}
	g := got.Total
	if g.Tokens != want.Tokens || g.Requests != want.Requests || g.WorkSeconds != want.WorkSeconds || !g.First.Equal(want.First) || !g.Last.Equal(want.Last) || len(g.Models) != len(want.Models) {
		t.Errorf("ReadDir total %+v, ReadTranscripts %+v", g, want)
	}
	if len(got.Sessions) != 2 || got.Sessions[0].File != "projects/-work/s1.jsonl" || got.Sessions[1].File != "projects/-work/s1/subagents/a.jsonl" {
		t.Errorf("sessions: %+v", got.Sessions)
	}
}

func TestReadDirCountsADuplicateOnceAcrossFilesAndPricesEachRequest(t *testing.T) {
	got, err := usage.ReadDir(fixture(t), time.Time{}, table(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total.Requests != 4 || got.Total.Tokens.Output != 200+50+9+3 {
		t.Errorf("requests %d, tokens %+v", got.Total.Requests, got.Total.Tokens)
	}
	// s1: sonnet 10 in, 1000 cache write (5m), 5000 read, 200 out; opus 5 in, 6000 read, 50 out
	sonnet := (10*2 + 1000*2*1.25 + 5000*0.10 + 200*10) / 1e6
	opus := (5*4 + 6000*0.20 + 50*20) / 1e6
	haiku := (1*0.10 + 9*0.50) / 1e6
	if math.Abs(got.CostUSD-(sonnet+opus+haiku)) > 1e-12 {
		t.Errorf("cost %v, want %v", got.CostUSD, sonnet+opus+haiku)
	}
	if len(got.Unpriced) != 1 || got.Unpriced[0] != "claude-mystery-9" {
		t.Errorf("unpriced = %v", got.Unpriced)
	}
	if len(got.Sessions) != 3 {
		t.Fatalf("sessions: %+v", got.Sessions)
	}
	other := got.Sessions[0]
	if other.File != "projects/-other/s2.jsonl" || other.Transcript.Tokens.Total() != 10 || other.CostUSD != 0 || len(other.Unpriced) != 1 {
		t.Errorf("unpriced session: %+v", other)
	}
}

func TestSinceCountsOnlyEntriesAtOrAfterIt(t *testing.T) {
	at := time.Date(2026, 11, 25, 14, 20, 0, 0, time.UTC) // exactly r2's time
	got, err := usage.ReadDir(fixture(t), at, table(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total.Requests != 3 || got.Total.Tokens.Input != 5+1+7 {
		t.Errorf("requests %d tokens %+v", got.Total.Requests, got.Total.Tokens)
	}
	if !got.Total.First.Equal(at) {
		t.Errorf("first = %v", got.Total.First)
	}
	// 14:20:00 -> 14:30:00 is 10 minutes, then 14:30:01
	if got.Total.WorkSeconds != 601 {
		t.Errorf("work = %d", got.Total.WorkSeconds)
	}
	late, err := usage.ReadDir(fixture(t), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), table(t))
	if err != nil || len(late.Sessions) != 1 || late.Total.Requests != 1 {
		t.Errorf("december: %+v %v", late, err)
	}
	none, err := usage.ReadDir(fixture(t), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), table(t))
	if err != nil || len(none.Sessions) != 0 || none.Total.Requests != 0 {
		t.Errorf("nothing after: %+v %v", none, err)
	}
}

func TestCacheWritesAreSplitByLifetime(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "p", "s.jsonl"),
		`{"type":"assistant","timestamp":"2026-11-25T14:00:00Z","requestId":"a","message":{"id":"a","model":"claude-sonnet-5-5","usage":{"cache_creation_input_tokens":3000000,"cache_creation":{"ephemeral_5m_input_tokens":1000000,"ephemeral_1h_input_tokens":2000000}}}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-11-25T14:00:01Z","requestId":"b","message":{"id":"b","model":"claude-sonnet-5-5","usage":{"cache_creation_input_tokens":1000000}}}`+"\n")
	got, err := usage.ReadDir(dir, time.Time{}, table(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2.5 + 8.0 + 2.5; math.Abs(got.CostUSD-want) > 1e-9 { // 1M@5m + 2M@1h, and 1M with no split as 5m
		t.Errorf("cost %v, want %v", got.CostUSD, want)
	}
	if got.Total.Tokens.CacheWrite != 4000000 {
		t.Errorf("cache write %d", got.Total.Tokens.CacheWrite)
	}
}

func TestADirectoryThatIsNotATranscriptsFolderIsAnErrorNamingIt(t *testing.T) {
	empty := t.TempDir()
	for _, dir := range []string{empty, filepath.Join(empty, "missing")} {
		_, err := usage.ReadDir(dir, time.Time{}, table(t))
		if !errors.Is(err, usage.ErrNotTranscripts) || err.Error()[:len(dir)] != dir {
			t.Errorf("%s: %v", dir, err)
		}
	}
}

func cwdReq(ts, id, cwd, model string, out int) string {
	c := ""
	if cwd != "" {
		c = `"cwd":"` + cwd + `",`
	}
	return `{"type":"assistant","timestamp":"` + ts + `",` + c + `"requestId":"r` + id + `","message":{"id":"m` + id + `","model":"` + model + `","usage":{"input_tokens":10,"output_tokens":` + itoa(out) + `}}}` + "\n"
}

func itoa(n int) string {
	return string(rune('0'+n/100%10)) + string(rune('0'+n/10%10)) + string(rune('0'+n%10))
}

func TestASessionIsSplitByWorkingDirectoryInOrderOfFirstAppearance(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "-h-proj", "s1.jsonl"),
		cwdReq("2026-11-25T10:00:00Z", "1", "/h/proj", "claude-sonnet-5-5", 100)+
			cwdReq("2026-11-25T10:01:00Z", "2", "/h/proj-two/sub", "claude-opus-5-5", 200)+
			// the same request repeated on another line counts once, under the first cwd
			cwdReq("2026-11-25T10:01:01Z", "2", "/h/elsewhere", "claude-opus-5-5", 200)+
			cwdReq("2026-11-25T10:02:00Z", "3", "/h/proj", "claude-sonnet-5-5", 100)+
			cwdReq("2026-11-25T10:03:00Z", "4", "", "claude-sonnet-5-5", 100))
	d, err := usage.ReadDir(dir, time.Time{}, table(t))
	if err != nil || len(d.Sessions) != 1 {
		t.Fatalf("%v %+v", err, d)
	}
	s := d.Sessions[0]
	if len(s.CWDs) != 3 || s.CWDs[0].CWD != "/h/proj" || s.CWDs[1].CWD != "/h/proj-two/sub" || s.CWDs[2].CWD != "" {
		t.Fatalf("cwds: %+v", s.CWDs)
	}
	if s.CWDs[0].Requests != 2 || s.CWDs[1].Requests != 1 || s.CWDs[2].Requests != 1 {
		t.Errorf("requests: %+v", s.CWDs)
	}
	var reqs int
	var cost float64
	for _, c := range s.CWDs {
		reqs += c.Requests
		cost += c.CostUSD
	}
	if reqs != s.Transcript.Requests || math.Abs(cost-s.CostUSD) > 1e-12 || s.CostUSD == 0 {
		t.Errorf("parts %d / %v, session %d / %v", reqs, cost, s.Transcript.Requests, s.CostUSD)
	}
	if s.CWDs[1].CostUSD <= s.CWDs[0].CostUSD/2 {
		t.Errorf("the opus request should cost more than one sonnet request: %+v", s.CWDs)
	}
}

func TestSinceIsAppliedBeforeTheCWDSplit(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "-h", "s1.jsonl"),
		cwdReq("2026-11-01T10:00:00Z", "1", "/h/old", "claude-sonnet-5-5", 100)+
			cwdReq("2026-12-01T10:00:00Z", "2", "/h/new", "claude-sonnet-5-5", 100))
	d, err := usage.ReadDir(dir, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), table(t))
	if err != nil || len(d.Sessions) != 1 {
		t.Fatalf("%v %+v", err, d)
	}
	s := d.Sessions[0]
	if len(s.CWDs) != 1 || s.CWDs[0].CWD != "/h/new" || s.CWDs[0].Requests != 1 || math.Abs(s.CWDs[0].CostUSD-s.CostUSD) > 1e-12 {
		t.Errorf("cwds: %+v", s.CWDs)
	}
}
