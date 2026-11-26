package inject_test

import (
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/inject"
	"github.com/charlesrobsampson/rig/internal/timeline"
)

func TestEveryMessageIsOneTimelineEntryWithoutItsText(t *testing.T) {
	f := newFixture(t, domain.AgentIdle)
	f.in = inject.New(inject.Options{Dir: f.dir, States: f.states, Backend: f.term, Readiness: f.ready, Clock: f.clock, Timeline: timeline.New(f.clock.Now)})
	f.send("a secret plan")
	f.setAgentState(domain.AgentBlocked)
	f.send("second")

	entries, err := timeline.Read(f.dir+"/demo", 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if e := entries[0]; e.Kind != timeline.MessageSent || e.Seq != 1 || e.Service != "api" || e.By != "human" || string(e.Data) != `{"service":"api","from":"human","delivered":true}` {
		t.Errorf("first = %+v (%s)", e, e.Data)
	}
	if e := entries[1]; e.Seq != 2 || !strings.Contains(string(e.Data), `"delivered":false`) {
		t.Errorf("second = %+v (%s)", e, e.Data)
	}
	for _, e := range entries {
		if strings.Contains(string(e.Data), "secret") {
			t.Errorf("the text of a message is in the timeline: %s", e.Data)
		}
	}
}
