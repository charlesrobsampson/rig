package supervisor_test

import (
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/supervisor"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

func TestEveryJournaledRecordIsOneTimelineEntryWithAnExcerpt(t *testing.T) {
	f := newFixture(t)
	f.sup = supervisor.New(supervisor.Options{
		Dir: f.dir, States: f.states, Backend: f.term, Bus: f.bus, Clock: f.clock, Timeline: timeline.New(f.clock.Now),
		Interval: time.Second, HookTTL: time.Minute, MaxBackoff: 8 * time.Second,
	})
	f.launch("demo", "api")
	long := strings.Repeat("x", 500)
	appendTo(t, f.eventsFile("demo", "api"), `{"kind":"note","text":"`+long+`"}`+"\n"+`{"kind":"Bad Kind"}`+"\n"+`{"kind":"feedback","text":"short"}`+"\n")
	f.poll()
	f.poll() // the same lines are not journaled, so not recorded, twice

	entries, err := timeline.Read(f.dir+"/demo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v, want one per journal record (including the rejection)", entries)
	}
	for i, e := range entries {
		if e.Seq != i+1 || e.Kind != timeline.JournalRecorded {
			t.Errorf("entry %d = %+v", i, e)
		}
	}
	if entries[0].By != "agent:api" || entries[0].Service != "api" || !strings.Contains(string(entries[0].Data), `"kind":"note"`) || strings.Contains(string(entries[0].Data), long) || len(entries[0].Data) > 300 {
		t.Errorf("long record = %+v (%d bytes of data)", entries[0], len(entries[0].Data))
	}
	if entries[1].By != "rig" || !strings.Contains(string(entries[1].Data), `"kind":"record.rejected"`) {
		t.Errorf("rejection = %+v", entries[1])
	}
	if string(entries[2].Data) != `{"kind":"feedback","text":"short"}` {
		t.Errorf("short record data = %s", entries[2].Data)
	}
}
