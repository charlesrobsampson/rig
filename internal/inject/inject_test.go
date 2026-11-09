package inject_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/inject"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var (
	ctx   = context.Background()
	epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

type readyFlag struct {
	mu sync.Mutex
	on bool
}

func (r *readyFlag) Ready(string, string) bool { r.mu.Lock(); defer r.mu.Unlock(); return r.on }
func (r *readyFlag) set(v bool)                { r.mu.Lock(); r.on = v; r.mu.Unlock() }

type fixture struct {
	t      *testing.T
	dir    string
	clock  *testkit.FakeClock
	states *state.Manager
	term   *testkit.FakeTerminal
	ready  *readyFlag
	in     *inject.Injector
	pane   terminal.PaneID
}

// newFixture has a ticket "demo" whose service "api" runs an agent in the given state,
// already reported ready by its hooks.
func newFixture(t *testing.T, agent domain.AgentState) *fixture {
	t.Helper()
	clock := testkit.NewFakeClock(epoch)
	f := &fixture{t: t, dir: testkit.RealTempDir(t), clock: clock, states: state.NewManager(clock), term: testkit.NewFakeTerminal(), ready: &readyFlag{on: true}}
	f.in = inject.New(inject.Options{Dir: f.dir, States: f.states, Backend: f.term, Readiness: f.ready, Clock: clock})
	id, err := f.term.Spawn(ctx, terminal.SpawnSpec{Ticket: "demo", Service: "api", Dir: f.dir, Command: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	f.pane = id
	os.MkdirAll(filepath.Join(f.dir, "demo"), 0o700)
	err = f.states.Create(filepath.Join(f.dir, "demo", "state.json"), state.State{TicketID: "demo", Services: map[string]state.ServiceState{
		"api": {Agent: state.Agent{Backend: "fake", PaneID: string(id), State: agent}},
		"web": {},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) setAgentState(st domain.AgentState) {
	f.t.Helper()
	err := f.states.Update(filepath.Join(f.dir, "demo", "state.json"), func(s *state.State) error {
		ss := s.Services["api"]
		ss.Agent.State = st
		s.Services["api"] = ss
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

type outcome struct {
	res api.AgentMessageResult
	err error
}

// sendAsync starts a Send and returns a way to wait for it.
func (f *fixture) sendAsync(service, text string) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		res, err := f.in.Send(ctx, api.AgentMessageParams{Ticket: "demo", Service: service, Text: text}, "human")
		done <- outcome{res, err}
	}()
	return done
}

// finish moves the fake clock forward until the send is over.
func (f *fixture) finish(done <-chan outcome) outcome {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case o := <-done:
			return o
		default:
		}
		if time.Now().After(deadline) {
			f.t.Fatal("the send never finished")
		}
		f.clock.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

func (f *fixture) send(text string) outcome { return f.finish(f.sendAsync("api", text)) }

func (f *fixture) logEntries() []map[string]any {
	f.t.Helper()
	file, err := os.Open(filepath.Join(f.dir, "demo", "messages.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	var out []map[string]any
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			f.t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func apiCode(t *testing.T, err error) api.ErrorCode {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want an *api.Error", err)
	}
	return e.Code
}

func TestTheTextIsTypedExactlyThenSubmittedSeparately(t *testing.T) {
	tests := []struct{ name, text string }{
		{"one line", "please rerun the tests"},
		{"several lines", "first line\nsecond line\n\nfourth"},
		{"quotes and shell characters", `it's "quoted" $HOME ` + "`id` $(id) \\n ; && | *"},
		{"unicode", "héllo ✓ 日本語"},
		{"tabs", "a\tb"},
		{"a leading dash", "-n --flag"},
		{"words that are key names", "Enter C-c Escape"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, domain.AgentIdle)
			o := f.send(tt.text)
			if o.err != nil {
				t.Fatal(o.err)
			}
			want := []testkit.Input{{Text: "[rig] " + tt.text}, {Submit: true}}
			got := f.term.Input(f.pane)
			if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				t.Errorf("input = %v, want text then Enter: %v", got, want)
			}
			if o.res.Sent != "[rig] "+tt.text || o.res.Service != "api" {
				t.Errorf("result = %+v", o.res)
			}
		})
	}
}

func TestControlCharactersAreEscapedNotTyped(t *testing.T) {
	tests := []struct{ name, text, want string }{
		{"escape ends a paste", "a\x1b[201~b", `a\x1b[201~b`},
		{"carriage return would press Enter", "a\rb", `a\rb`},
		{"ctrl-c", "stop\x03", `stop\x03`},
		{"nul and bell", "a\x00b\x07", `a\x00b\x07`},
		{"delete", "a\x7fb", `a\x7fb`},
		{"a C1 control", "a\u009bb", `a\x9bb`},
		{"invalid utf-8", "a\xffb", `a\xffb`},
		{"newline and tab survive", "a\nb\tc", "a\nb\tc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inject.Escape(tt.text); got != tt.want {
				t.Errorf("Escape(%q) = %q, want %q", tt.text, got, tt.want)
			}
			f := newFixture(t, domain.AgentIdle)
			if o := f.send(tt.text); o.err != nil {
				t.Fatal(o.err)
			}
			typed := f.term.Input(f.pane)[0].Text
			for _, r := range strings.TrimPrefix(typed, "[rig] ") {
				if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
					t.Errorf("typed text contains control character %q: %q", r, typed)
				}
			}
		})
	}
}

func TestNothingAtAllIsTypedIntoABlockedAgent(t *testing.T) {
	t.Run("the hooks say blocked", func(t *testing.T) {
		f := newFixture(t, domain.AgentBlocked)
		o := f.send("hello")
		if apiCode(t, o.err) != api.CodeBlocked || !strings.Contains(o.err.Error(), "permission prompt") {
			t.Errorf("error = %v", o.err)
		}
		if got := f.term.Input(f.pane); len(got) != 0 {
			t.Errorf("input = %v, want nothing (not even Enter)", got)
		}
	})
	t.Run("only the screen says blocked", func(t *testing.T) {
		f := newFixture(t, domain.AgentWorking)
		f.term.SetHeuristic(f.pane, domain.AgentBlocked)
		o := f.send("hello")
		if apiCode(t, o.err) != api.CodeBlocked || len(f.term.Input(f.pane)) != 0 {
			t.Errorf("error = %v, input = %v", o.err, f.term.Input(f.pane))
		}
	})
	t.Run("a prompt appears while the text is going in", func(t *testing.T) {
		f := newFixture(t, domain.AgentWorking)
		done := f.sendAsync("api", "hello")
		deadline := time.Now().Add(5 * time.Second)
		for len(f.term.Input(f.pane)) == 0 { // the text is in, the pause has begun
			if time.Now().After(deadline) {
				t.Fatal("the text was never typed")
			}
			time.Sleep(time.Millisecond)
		}
		f.setAgentState(domain.AgentBlocked)
		o := f.finish(done)
		if apiCode(t, o.err) != api.CodeBlocked || !strings.Contains(o.err.Error(), "not submitted") {
			t.Errorf("error = %v", o.err)
		}
		for _, in := range f.term.Input(f.pane) {
			if in.Submit {
				t.Error("Enter was pressed on a prompt")
			}
		}
	})
}

func TestMessagesGoToWorkingAndIdleAgents(t *testing.T) {
	for _, st := range []domain.AgentState{domain.AgentWorking, domain.AgentIdle} {
		f := newFixture(t, st)
		if o := f.send("hi"); o.err != nil || len(f.term.Input(f.pane)) != 2 {
			t.Errorf("%s: %v, input %v", st, o.err, f.term.Input(f.pane))
		}
	}
}

func TestAMessageWaitsForAFreshAgentToBeReady(t *testing.T) {
	t.Run("until its hooks report ready", func(t *testing.T) {
		f := newFixture(t, domain.AgentUnknown)
		f.ready.set(false)
		done := f.sendAsync("api", "hello")
		for range 40 { // ten seconds of fake time pass
			f.clock.Advance(250 * time.Millisecond)
			time.Sleep(time.Millisecond)
		}
		if got := f.term.Input(f.pane); len(got) != 0 {
			t.Fatalf("typed %v into an agent that was not ready", got)
		}
		f.ready.set(true)
		if o := f.finish(done); o.err != nil || len(f.term.Input(f.pane)) != 2 {
			t.Errorf("after ready: %v, input %v", o.err, f.term.Input(f.pane))
		}
	})
	t.Run("a recorded idle state counts after a daemon restart", func(t *testing.T) {
		f := newFixture(t, domain.AgentIdle)
		f.ready.set(false)
		if o := f.send("hello"); o.err != nil {
			t.Errorf("error = %v", o.err)
		}
	})
	t.Run("giving up after the timeout, typing nothing", func(t *testing.T) {
		f := newFixture(t, domain.AgentUnknown)
		f.ready.set(false)
		o := f.send("hello")
		if apiCode(t, o.err) != api.CodeUnavailable || !strings.Contains(o.err.Error(), "ready") || len(f.term.Input(f.pane)) != 0 {
			t.Errorf("error = %v, input %v", o.err, f.term.Input(f.pane))
		}
	})
	t.Run("blocking while it waits refuses", func(t *testing.T) {
		f := newFixture(t, domain.AgentUnknown)
		f.ready.set(false)
		done := f.sendAsync("api", "hello")
		f.clock.Advance(time.Second)
		f.setAgentState(domain.AgentBlocked)
		o := f.finish(done)
		if apiCode(t, o.err) != api.CodeBlocked || len(f.term.Input(f.pane)) != 0 {
			t.Errorf("error = %v, input %v", o.err, f.term.Input(f.pane))
		}
	})
}

func TestMessagesToAgentsThatAreNotThere(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fixture)
		code  api.ErrorCode
	}{
		{"the process exited, pane kept", func(f *fixture) { f.term.Exit(f.pane, 1) }, api.CodeNotRunning},
		{"the pane vanished", func(f *fixture) { f.term.Vanish(f.pane) }, api.CodeNotRunning},
		{"recorded as exited", func(f *fixture) { f.setAgentState(domain.AgentExited) }, api.CodeNotRunning},
		{"the backend is down", func(f *fixture) { f.term.Down(terminal.ErrUnavailable) }, api.CodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, domain.AgentIdle)
			tt.setup(f)
			if o := f.send("hello"); apiCode(t, o.err) != tt.code {
				t.Errorf("error = %v, want %s", o.err, tt.code)
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	f := newFixture(t, domain.AgentIdle)
	tests := []struct {
		name string
		p    api.AgentMessageParams
		code api.ErrorCode
		msg  string
	}{
		{"unknown ticket", api.AgentMessageParams{Ticket: "ghost", Service: "api", Text: "x"}, api.CodeNotFound, "ghost"},
		{"unknown service", api.AgentMessageParams{Ticket: "demo", Service: "nope", Text: "x"}, api.CodeNotFound, "nope"},
		{"a service with no agent", api.AgentMessageParams{Ticket: "demo", Service: "web", Text: "x"}, api.CodeNotRunning, "start the ticket"},
		{"empty text", api.AgentMessageParams{Ticket: "demo", Service: "api", Text: ""}, api.CodeInvalid, "text"},
		{"blank text", api.AgentMessageParams{Ticket: "demo", Service: "api", Text: " \n "}, api.CodeInvalid, "text"},
		{"too long", api.AgentMessageParams{Ticket: "demo", Service: "api", Text: strings.Repeat("x", 17*1024)}, api.CodeInvalid, "at most"},
		{"a path as the ticket", api.AgentMessageParams{Ticket: "../demo", Service: "api", Text: "x"}, api.CodeInvalid, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan outcome, 1)
			go func() { res, err := f.in.Send(ctx, tt.p, "human"); done <- outcome{res, err} }()
			o := f.finish(done)
			if apiCode(t, o.err) != tt.code || !strings.Contains(o.err.Error(), tt.msg) {
				t.Errorf("error = %v, want %s mentioning %q", o.err, tt.code, tt.msg)
			}
		})
	}
	if got := f.term.Input(f.pane); len(got) != 0 {
		t.Errorf("a bad request typed %v", got)
	}
}

func TestEveryMessageIsLoggedWithItsOutcome(t *testing.T) {
	f := newFixture(t, domain.AgentIdle)
	f.send("first\nline")
	f.setAgentState(domain.AgentBlocked)
	f.send("second")

	entries := f.logEntries()
	if len(entries) != 2 {
		t.Fatalf("log = %v", entries)
	}
	first, second := entries[0], entries[1]
	if first["text"] != "first\nline" || first["delivered"] != true || first["service"] != "api" || first["from"] != "human" || first["at"] != "2026-11-09T12:00:00Z" && first["at"] == nil {
		t.Errorf("first entry = %v", first)
	}
	if second["text"] != "second" || second["delivered"] != false || !strings.Contains(second["reason"].(string), "permission prompt") {
		t.Errorf("second entry = %v", second)
	}
	if info, err := os.Stat(filepath.Join(f.dir, "demo", "messages.jsonl")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("log file: %v, %v", info, err)
	}
}

func TestConcurrentMessagesToOneAgentDoNotInterleave(t *testing.T) {
	f := newFixture(t, domain.AgentIdle)
	var dones []<-chan outcome
	for _, text := range []string{"one", "two", "three", "four"} {
		dones = append(dones, f.sendAsync("api", text))
	}
	for _, d := range dones {
		if o := f.finish(d); o.err != nil {
			t.Fatal(o.err)
		}
	}
	input := f.term.Input(f.pane)
	if len(input) != 8 {
		t.Fatalf("input = %v", input)
	}
	for i := 0; i < len(input); i += 2 {
		if input[i].Submit || !strings.HasPrefix(input[i].Text, "[rig] ") || !input[i+1].Submit {
			t.Errorf("messages interleaved at %d: %v", i, input)
		}
	}
}

func TestACancelledContextStopsAWaitingMessage(t *testing.T) {
	f := newFixture(t, domain.AgentUnknown)
	f.ready.set(false)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := f.in.Send(cctx, api.AgentMessageParams{Ticket: "demo", Service: "api", Text: "x"}, "human")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || len(f.term.Input(f.pane)) != 0 {
			t.Errorf("error = %v, input %v", err, f.term.Input(f.pane))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the message kept waiting after cancellation")
	}
}
