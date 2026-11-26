package timeline

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newLog(dir string) *Log {
	return New(func() time.Time { return time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC) })
}

func TestMissingFileReadsAsEmpty(t *testing.T) {
	got, err := Read(t.TempDir(), 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestSeqIsDenseAndOrderedUnderConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	l := newLog(dir)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Append(dir, Entry{Kind: PhaseMoved, Data: Data(PhaseMovedData{From: "plan", To: "implement"})}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := Read(dir, 0)
	if err != nil || len(got) != 50 {
		t.Fatalf("got %d entries, %v", len(got), err)
	}
	for i, e := range got {
		if e.Seq != i+1 {
			t.Fatalf("entry %d has seq %d", i, e.Seq)
		}
	}
}

func TestSinceKeepsLaterEntries(t *testing.T) {
	dir := t.TempDir()
	l := newLog(dir)
	for i := 0; i < 5; i++ {
		l.Append(dir, Entry{Kind: TicketStarted})
	}
	got, _ := Read(dir, 3)
	if len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 5 {
		t.Fatalf("got %+v", got)
	}
}

func TestSeqContinuesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	newLog(dir).Append(dir, Entry{Kind: TicketStarted})
	newLog(dir).Append(dir, Entry{Kind: TicketStopped})
	got, _ := Read(dir, 0)
	if len(got) != 2 || got[1].Seq != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestTornLastLineIsRepaired(t *testing.T) {
	dir := t.TempDir()
	newLog(dir).Append(dir, Entry{Kind: TicketStarted})
	f, _ := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"seq":2,"at":"2026-11`)
	f.Close()
	e, err := newLog(dir).Append(dir, Entry{Kind: TicketStopped})
	if err != nil || e.Seq != 2 {
		t.Fatalf("got %+v, %v", e, err)
	}
	got, _ := Read(dir, 0)
	if len(got) != 2 || got[1].Kind != TicketStopped {
		t.Fatalf("got %+v", got)
	}
}

func TestNilLogIsANoOp(t *testing.T) {
	var l *Log
	if _, err := l.Append(t.TempDir(), Entry{Kind: TicketStarted}); err != nil {
		t.Fatal(err)
	}
}

func TestExcerptCutsOnARuneBoundary(t *testing.T) {
	s := Excerpt(string(make([]byte, 0)) + "é" + string(make([]rune, 0)))
	if s != "é" {
		t.Fatal(s)
	}
	long := ""
	for i := 0; i < 300; i++ {
		long += "é"
	}
	if got := Excerpt(long); len(got) > ExcerptBytes+3 || got == long {
		t.Fatalf("len %d", len(got))
	}
}

func TestOnAppendHearsEachWrittenEntryInSeqOrder(t *testing.T) {
	dir := t.TempDir()
	l := newLog(dir)
	var got []int
	l.OnAppend = func(d string, e Entry) {
		if d != dir {
			t.Errorf("dir %q", d)
		}
		got = append(got, e.Seq) // runs under the ticket lock, so no race
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.Append(dir, Entry{Kind: TicketStarted}) }()
	}
	wg.Wait()
	for i, s := range got {
		if s != i+1 {
			t.Fatalf("heard %v", got)
		}
	}
	if len(got) != 40 {
		t.Fatalf("heard %d", len(got))
	}
}
