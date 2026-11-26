package timeline

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAFailedWriteLeavesNoGapAndTheFragmentIsCutBeforeTheNextAppend(t *testing.T) {
	dir := t.TempDir()
	l := New(func() time.Time { return time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC) })
	var logged strings.Builder
	l.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	if _, err := l.Append(dir, Entry{Kind: TicketStarted}); err != nil {
		t.Fatal(err)
	}
	real := func(f *os.File, b []byte) error { _, err := f.Write(b); return err }
	l.write = func(f *os.File, b []byte) error { // disk full part way through the line
		f.Write(b[:len(b)/2])
		return errors.New("no space left on device")
	}
	if _, err := l.Append(dir, Entry{Kind: PhaseMoved}); err == nil {
		t.Fatal("the failed write was reported as a success")
	}
	if !strings.Contains(logged.String(), "no space left") || !strings.Contains(logged.String(), PhaseMoved) {
		t.Errorf("the failure was not logged with its kind: %q", logged.String())
	}
	l.write = real
	e, err := l.Append(dir, Entry{Kind: TicketStopped})
	if err != nil || e.Seq != 2 {
		t.Fatalf("next append = %+v, %v; want seq 2, no gap", e, err)
	}
	got, _ := Read(dir, 0)
	if len(got) != 2 || got[0].Kind != TicketStarted || got[1].Kind != TicketStopped || got[1].Seq != 2 {
		t.Fatalf("entries = %+v", got)
	}
}

func TestAnUnwritableTimelineFailsTheAppendAndIsLogged(t *testing.T) {
	var logged strings.Builder
	l := New(nil)
	l.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	if _, err := l.Append(t.TempDir()+"/missing/dir", Entry{Kind: SyncDone}); err == nil {
		t.Fatal("an unwritable directory was not an error")
	}
	if !strings.Contains(logged.String(), SyncDone) || !strings.Contains(logged.String(), "open timeline") {
		t.Errorf("log = %q", logged.String())
	}
}
