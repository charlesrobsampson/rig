package supervisor_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/supervisor"
)

type askedFor struct {
	ticket, service string
	to              domain.Phase
	reason          string
}

// withPhaseRequests rebuilds the fixture's supervisor with a hook that records what it
// is asked, refusing with refuse when set.
func (f *fixture) withPhaseRequests(refuse error) *[]askedFor {
	var asked []askedFor
	f.sup = supervisor.New(supervisor.Options{
		Dir: f.dir, States: f.states, Backend: f.term, Bus: f.bus, Clock: f.clock,
		Interval: time.Second, HookTTL: time.Minute, MaxBackoff: 8 * time.Second,
		PhaseRequest: func(ticket, service string, to domain.Phase, reason string) error {
			asked = append(asked, askedFor{ticket, service, to, reason})
			return refuse
		},
	})
	return &asked
}

func TestATransitionRequestRecordIsHandedOnFromEitherSandbox(t *testing.T) {
	f := newFixture(t)
	asked := f.withPhaseRequests(nil)
	f.launch("demo", "api")
	appendTo(t, f.eventsFile("demo", "api"), `{"kind":"transition.request","text":"plan ready","data":{"to":"implement","reason":"plan is ready"}}`+"\n")
	f.poll()
	if err := f.sup.Record(ctx, api.AgentRecordParams{Ticket: "demo", Service: "api", Kind: "transition.request", Data: json.RawMessage(`{"to":"test"}`)}); err != nil {
		t.Fatal(err)
	}

	want := []askedFor{{"demo", "api", domain.PhaseImplement, "plan is ready"}, {"demo", "api", domain.PhaseTest, ""}}
	if len(*asked) != 2 || (*asked)[0] != want[0] || (*asked)[1] != want[1] {
		t.Errorf("asked = %+v, want %+v", *asked, want)
	}
	if kinds(f.journal("demo")) != "transition.request,transition.request" {
		t.Errorf("journal = %s; the request itself is kept", kinds(f.journal("demo")))
	}
}

func TestABadTransitionRequestIsRejectedWithTheReason(t *testing.T) {
	cases := []struct {
		name, data string
		refuse     error
		want       string
	}{
		{"no to", `{"reason":"x"}`, nil, "to"},
		{"not a phase", `{"to":"daydream"}`, nil, "daydream"},
		{"refused", `{"to":"implement"}`, errors.New("ticket is already in implement"), "already in implement"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.withPhaseRequests(c.refuse)
			f.launch("demo", "api")
			appendTo(t, f.eventsFile("demo", "api"), `{"kind":"transition.request","data":`+c.data+`}`+"\n")
			f.poll()
			got := f.journal("demo")
			if kinds(got) != "transition.request,record.rejected" {
				t.Fatalf("journal = %s", kinds(got))
			}
			if r := got[1]; r.Source != api.JournalSourceRig || r.Service != "api" || !strings.Contains(r.Text, c.want) {
				t.Errorf("rejection = %+v, want text naming %q", r, c.want)
			}
		})
	}
}
