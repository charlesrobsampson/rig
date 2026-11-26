package supervisor_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/journal"
	"github.com/charlesrobsampson/rig/internal/supervisor"
)

func (f *fixture) journal(ticket string) []api.JournalRecord {
	f.t.Helper()
	got, err := journal.Read(filepath.Join(f.dir, ticket))
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// recordEvents drains the agent.record events published so far.
func (f *fixture) recordEvents() []api.Event {
	f.t.Helper()
	var out []api.Event
	for {
		select {
		case ev := <-f.sub.Events():
			if ev.Type == api.EventAgentRecord {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

func kinds(rs []api.JournalRecord) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Kind)
	}
	return strings.Join(out, ",")
}

func TestAContainerAgentsRecordIsJournaledWithRigsOwnStamps(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")
	appendTo(t, file, "working\n"+`{"kind":"feedback","text":"npm registry is blocked","data":{"host":"registry.npmjs.org"},"ticket":"other","service":"web","at":"2001-01-01T00:00:00Z","source":"rig"}`+"\n")
	f.poll()

	got := f.journal("demo")
	if len(got) != 1 {
		t.Fatalf("journal = %+v, want one record", got)
	}
	r := got[0]
	if r.Ticket != "demo" || r.Service != "api" || r.Source != api.JournalSourceContainer || !r.At.Equal(f.clock.Now()) {
		t.Errorf("stamps = %s/%s %s %v; rig's own values must win over the agent's", r.Ticket, r.Service, r.Source, r.At)
	}
	if r.Kind != "feedback" || r.Text != "npm registry is blocked" || string(r.Data) != `{"host":"registry.npmjs.org"}` {
		t.Errorf("record = %+v (data %s)", r, r.Data)
	}
	if f.agentState("demo", "api") != domain.AgentWorking {
		t.Errorf("state = %s; a record is not a state", f.agentState("demo", "api"))
	}
	ev := f.recordEvents()
	if len(ev) != 1 || ev[0].Ticket != "demo" || ev[0].Service != "api" {
		t.Fatalf("agent.record events = %+v", ev)
	}
	var d api.AgentRecordData
	if err := ev[0].DecodeData(&d); err != nil || d.Kind != "feedback" || d.Text != "npm registry is blocked" {
		t.Errorf("event data = %+v, %v", d, err)
	}
}

func TestARecordLineNeverMovesTheAgentsState(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")
	// first sight keeps only the last state: a record after it must not count as one
	appendTo(t, file, "working\nidle\n{\"kind\":\"note\"}\n{broken\n")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentIdle {
		t.Errorf("state = %s, want idle", got)
	}
	appendTo(t, file, "working\n{\"kind\":\"note\"}\n")
	f.poll()
	if got := f.agentState("demo", "api"); got != domain.AgentWorking {
		t.Errorf("state = %s, want working", got)
	}
}

func TestBadRecordsAreDroppedAndNotedByRig(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")
	appendTo(t, file, "{broken\n"+`{"kind":"Bad Kind"}`+"\n"+`{"kind":"note","data":[1]}`+"\n"+`{"kind":"ok"}`+"\n")
	f.poll()

	got := f.journal("demo")
	if kinds(got) != "record.rejected,record.rejected,record.rejected,ok" {
		t.Fatalf("journal kinds = %s", kinds(got))
	}
	for _, r := range got[:3] {
		if r.Source != api.JournalSourceRig || r.Text == "" || r.Service != "api" {
			t.Errorf("rejection = %+v; rig writes why, for the agent's service", r)
		}
	}
	var d struct {
		Line string `json:"line"`
	}
	if err := json.Unmarshal(got[0].Data, &d); err != nil || d.Line != "{broken" {
		t.Errorf("rejection data = %s; it keeps the start of the line", got[0].Data)
	}
}

func TestAHugeBadLineIsQuotedOnlyInPart(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	appendTo(t, f.eventsFile("demo", "api"), "{"+strings.Repeat("x", journal.MaxLineBytes)+"\n")
	f.poll()
	got := f.journal("demo")
	if len(got) != 1 || len(got[0].Data) > 1024 {
		t.Errorf("journal = %d records, data %d bytes; a rejection quotes at most a short prefix", len(got), len(got[0].Data))
	}
}

func TestARestartedDaemonJournalsEveryRecordExactlyOnce(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")
	appendTo(t, file, "{\"kind\":\"one\"}\nworking\n{\"kind\":\"two\"}\n")
	f.poll()
	f.poll()

	// a new supervisor knows nothing in memory, as after a restart
	restarted := supervisor.New(supervisor.Options{Dir: f.dir, States: f.states, Backend: f.term, Bus: f.bus, Clock: f.clock})
	appendTo(t, file, "{\"kind\":\"three\"}\n{\"kind\":\"fo")
	if err := restarted.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	appendTo(t, file, "ur\"}\n")
	restarted.Poll(ctx)
	restarted.Poll(ctx)
	if got := kinds(f.journal("demo")); got != "one,two,three,four" {
		t.Errorf("journal kinds = %s, want each record once, a half line only when complete", got)
	}
}

func TestATruncatedEventsFileIsJournaledFromTheStart(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	file := f.eventsFile("demo", "api")
	appendTo(t, file, "{\"kind\":\"one\"}\n{\"kind\":\"two\"}\n")
	f.poll()
	os.WriteFile(file, []byte("{\"kind\":\"new\"}\n"), 0o600)
	f.poll()
	if got := kinds(f.journal("demo")); got != "one,two,new" {
		t.Errorf("journal kinds = %s", got)
	}
}

func TestAHostAgentRecordsThroughTheAPI(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "api")
	err := f.sup.Record(ctx, api.AgentRecordParams{Ticket: "demo", Service: "api", Kind: "verdict", Text: "PASS", Data: json.RawMessage(`{"role":"reviewer"}`)})
	if err != nil {
		t.Fatal(err)
	}
	got := f.journal("demo")
	if len(got) != 1 || got[0].Source != api.JournalSourceHost || got[0].Kind != "verdict" || string(got[0].Data) != `{"role":"reviewer"}` || !got[0].At.Equal(f.clock.Now()) {
		t.Errorf("journal = %+v", got)
	}
	if ev := f.recordEvents(); len(ev) != 1 {
		t.Errorf("agent.record events = %d", len(ev))
	}

	tests := []struct {
		name string
		p    api.AgentRecordParams
		code api.ErrorCode
	}{
		{"bad kind", api.AgentRecordParams{Ticket: "demo", Service: "api", Kind: "Nope"}, api.CodeInvalid},
		{"data not an object", api.AgentRecordParams{Ticket: "demo", Service: "api", Kind: "note", Data: json.RawMessage(`"x"`)}, api.CodeInvalid},
		{"bad ticket id", api.AgentRecordParams{Ticket: "../x", Service: "api", Kind: "note"}, api.CodeInvalid},
		{"unknown ticket", api.AgentRecordParams{Ticket: "nope", Service: "api", Kind: "note"}, api.CodeNotFound},
		{"unknown service", api.AgentRecordParams{Ticket: "demo", Service: "web", Kind: "note"}, api.CodeNotFound},
	}
	for _, tt := range tests {
		var apiErr *api.Error
		if err := f.sup.Record(ctx, tt.p); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
			t.Errorf("%s: err = %v, want %s", tt.name, err, tt.code)
		}
	}
	if len(f.journal("demo")) != 1 {
		t.Error("a refused record was journaled")
	}
}

func TestTheJournalIsListedAcrossTicketsOrFiltered(t *testing.T) {
	f := newFixture(t)
	f.launch("a", "api")
	f.launch("b", "web")
	f.sup.Record(ctx, api.AgentRecordParams{Ticket: "a", Service: "api", Kind: "feedback", Text: "a1"})
	f.clock.Advance(time.Minute)
	f.sup.Record(ctx, api.AgentRecordParams{Ticket: "b", Service: "web", Kind: "feedback", Text: "b1"})
	f.clock.Advance(time.Minute)
	f.sup.Record(ctx, api.AgentRecordParams{Ticket: "a", Service: "api", Kind: "note", Text: "a2"})
	os.MkdirAll(filepath.Join(f.dir, "empty"), 0o700) // a ticket without a journal

	texts := func(p api.JournalListParams) string {
		t.Helper()
		res, err := f.sup.Journal(p)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range res.Records {
			out = append(out, r.Text)
		}
		return strings.Join(out, ",")
	}
	if got := texts(api.JournalListParams{}); got != "a1,b1,a2" {
		t.Errorf("all = %s, want every ticket, oldest first", got)
	}
	if got := texts(api.JournalListParams{Kind: "feedback"}); got != "a1,b1" {
		t.Errorf("feedback = %s", got)
	}
	if got := texts(api.JournalListParams{Ticket: "a", Limit: 1}); got != "a2" {
		t.Errorf("newest of a = %s", got)
	}
	res, err := f.sup.Journal(api.JournalListParams{Ticket: "empty"})
	if err != nil || len(res.Records) != 0 || res.Records == nil {
		t.Errorf("a ticket without a journal = %+v, %v; want an empty list", res, err)
	}

	var apiErr *api.Error
	if _, err := f.sup.Journal(api.JournalListParams{Ticket: "nope"}); !errors.As(err, &apiErr) || apiErr.Code != api.CodeNotFound {
		t.Errorf("unknown ticket: %v", err)
	}
	if _, err := f.sup.Journal(api.JournalListParams{Limit: -1}); !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalid {
		t.Errorf("negative limit: %v", err)
	}
}

func TestARejectedCheckerRecordIsToldToThatCheckerAndNotTheCoder(t *testing.T) {
	f := newFixture(t)
	type told struct{ service, text string }
	var got []told
	f.sup = supervisor.New(supervisor.Options{
		Dir: f.dir, States: f.states, Backend: f.term, Bus: f.bus, Clock: f.clock,
		Interval: time.Second, HookTTL: time.Minute, MaxBackoff: 8 * time.Second,
		Tell: func(ticket, service, text string) { got = append(got, told{service, text}) },
	})
	for _, svc := range []string{"api", "review", "tester"} {
		f.launch("demo", svc)
		appendTo(t, f.eventsFile("demo", svc), "{broken "+svc+"\n")
	}
	f.poll()
	if len(got) != 2 || got[0].service == "api" || got[1].service == "api" {
		t.Fatalf("told %+v; want the reviewer and the tester only", got)
	}
	for _, g := range got {
		for _, want := range []string{"rejected", "{broken " + g.service, "quoted heredoc", "rig agent-event --file"} {
			if !strings.Contains(g.text, want) {
				t.Errorf("%s was told %q; want %q in it", g.service, g.text, want)
			}
		}
	}
}

func TestAnIdleAgentRecordsSinceWhenAndClearsItWhenItWorks(t *testing.T) {
	f := newFixture(t)
	f.launch("demo", "review")
	f.mustReport("demo", "review", "idle")
	st, _ := f.states.Load(filepath.Join(f.dir, "demo", "state.json"))
	if got := st.Services["review"].Agent.IdleSince; !got.Equal(f.clock.Now()) {
		t.Errorf("idle_since = %v, want %v", got, f.clock.Now())
	}
	f.mustReport("demo", "review", "working")
	st, _ = f.states.Load(filepath.Join(f.dir, "demo", "state.json"))
	if got := st.Services["review"].Agent.IdleSince; !got.IsZero() {
		t.Errorf("idle_since = %v while working", got)
	}
}
