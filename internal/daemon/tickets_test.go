package daemon_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

func ticketsHarness(t *testing.T) (*harness, string) {
	t.Helper()
	dir := filepath.Join(shortDir(t), "tickets")
	mgr := state.NewManager(testkit.NewFakeClock(epoch))
	reg := registry.New(dir, mgr)
	h := start(t, func(s *daemon.Server) { daemon.RegisterTicketHandlers(s, reg) })
	return h, dir
}

func writeTicket(t *testing.T, dir, id string) {
	t.Helper()
	tdir := filepath.Join(dir, id)
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "id: " + id + "\ntitle: Title of " + id + "\nservices:\n  - {name: api, repo: /repos/api, branch: feat/" + id + "}\n"
	if err := os.WriteFile(filepath.Join(tdir, "ticket.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := state.NewManager(testkit.NewFakeClock(epoch))
	if err := mgr.Create(filepath.Join(tdir, "state.json"), state.State{TicketID: id, Phase: domain.PhaseImplement}); err != nil {
		t.Fatal(err)
	}
}

func TestTicketListAndRescanReadTheDirectoryFresh(t *testing.T) {
	h, dir := ticketsHarness(t)
	c := dial(t, h.opts.SocketPath)
	c.hello(t)

	var empty api.TicketListResult
	json.Unmarshal(c.call(t, api.MethodTicketList, nil).Result, &empty)
	if empty.Tickets == nil || len(empty.Tickets) != 0 {
		t.Fatalf("empty list = %#v, want an empty array", empty.Tickets)
	}

	writeTicket(t, dir, "ticket-a")
	for _, method := range []string{api.MethodTicketList, api.MethodTicketRescan} {
		var got api.TicketListResult
		json.Unmarshal(c.call(t, method, nil).Result, &got)
		if len(got.Tickets) != 1 || got.Tickets[0].ID != "ticket-a" || got.Tickets[0].Phase != domain.PhaseImplement {
			t.Errorf("%s = %+v", method, got.Tickets)
		}
	}
}

func TestTicketGet(t *testing.T) {
	h, dir := ticketsHarness(t)
	writeTicket(t, dir, "ticket-a")
	c := dial(t, h.opts.SocketPath)
	c.hello(t)

	var got api.TicketGetResult
	resp := c.call(t, api.MethodTicketGet, api.TicketGetParams{ID: "ticket-a"})
	if err := json.Unmarshal(resp.Result, &got); err != nil || got.Ticket == nil || got.State == nil || got.Summary.Title != "Title of ticket-a" {
		t.Fatalf("get = %s, %v", resp.Result, err)
	}

	tests := []struct {
		name   string
		params any
		code   api.ErrorCode
	}{
		{"unknown ticket", api.TicketGetParams{ID: "ticket-zzz"}, api.CodeNotFound},
		{"an id that would escape the directory", api.TicketGetParams{ID: "../etc"}, api.CodeInvalid},
		{"missing id", api.TicketGetParams{}, api.CodeInvalid},
		{"params that are not an object", "just a string", api.CodeInvalid},
		{"no params at all", nil, api.CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr *api.Error
			if err := c.call(t, api.MethodTicketGet, tt.params).Err(); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

func TestTicketFile(t *testing.T) {
	h, dir := ticketsHarness(t)
	writeTicket(t, dir, "ticket-a")
	os.WriteFile(filepath.Join(dir, "ticket-a", "plan.md"), []byte("# Plan\n- [ ] x\n"), 0o600)
	c := dial(t, h.opts.SocketPath)
	c.hello(t)

	var got api.TicketFileResult
	resp := c.call(t, api.MethodTicketFile, api.TicketFileParams{Ticket: "ticket-a", Name: "plan.md"})
	if err := json.Unmarshal(resp.Result, &got); err != nil || got.Name != "plan.md" || got.Content != "# Plan\n- [ ] x\n" {
		t.Fatalf("file = %s, %v", resp.Result, err)
	}
	tests := []struct {
		name   string
		params any
		code   api.ErrorCode
	}{
		{"a file that does not exist", api.TicketFileParams{Ticket: "ticket-a", Name: "contract.md"}, api.CodeNotFound},
		{"an unknown ticket", api.TicketFileParams{Ticket: "ticket-zzz", Name: "plan.md"}, api.CodeNotFound},
		{"state.json is not readable", api.TicketFileParams{Ticket: "ticket-a", Name: "state.json"}, api.CodeInvalid},
		{"a path", api.TicketFileParams{Ticket: "ticket-a", Name: "../ticket-a/plan.md"}, api.CodeInvalid},
		{"an id that would escape", api.TicketFileParams{Ticket: "../etc", Name: "plan.md"}, api.CodeInvalid},
		{"no params", nil, api.CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr *api.Error
			if err := c.call(t, api.MethodTicketFile, tt.params).Err(); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

func createHarness(t *testing.T) (*harness, *events.Bus, string) {
	t.Helper()
	dir := filepath.Join(shortDir(t), "tickets")
	mgr := state.NewManager(testkit.NewFakeClock(epoch))
	bus := events.NewBus(testkit.NewFakeClock(epoch), 0)
	creator := registry.NewCreator(registry.CreatorOptions{
		Dir: dir, States: mgr, Sources: ticket.DefaultRegistry(),
		RepoCheck: func(string) error { return nil }, BranchPrefix: "rig/",
	})
	h := start(t, func(s *daemon.Server) { daemon.RegisterTicketCreateHandler(s, creator, bus) })
	return h, bus, dir
}

func TestTicketNewCreatesTheTicketAndAnnouncesIt(t *testing.T) {
	h, bus, dir := createHarness(t)
	sub := bus.Subscribe()
	defer sub.Close()
	<-sub.Events() // the subscribed ack

	c := dial(t, h.opts.SocketPath)
	c.hello(t)
	resp := c.call(t, api.MethodTicketNew, api.TicketNewParams{ID: "demo", Title: "Demo", Services: []api.ServiceSpec{{Name: "api", Repo: "/repos/api"}}})
	var got api.TicketNewResult
	if err := json.Unmarshal(resp.Result, &got); err != nil || got.Summary.ID != "demo" || got.Path != filepath.Join(dir, "demo") {
		t.Fatalf("result = %s, %v", resp.Result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo", "plan.md")); err != nil {
		t.Errorf("plan.md was not written: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != api.EventTicketCreated || ev.Ticket != "demo" {
			t.Errorf("event = %s for %q, want ticket.created for demo", ev.Type, ev.Ticket)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ticket.created event was published")
	}
}

func TestTicketNewErrors(t *testing.T) {
	h, bus, _ := createHarness(t)
	sub := bus.Subscribe()
	defer sub.Close()
	<-sub.Events()
	c := dial(t, h.opts.SocketPath)
	c.hello(t)
	ok := api.TicketNewParams{ID: "demo", Title: "Demo", Services: []api.ServiceSpec{{Name: "api", Repo: "/repos/api"}}}
	if resp := c.call(t, api.MethodTicketNew, ok); resp.Err() != nil {
		t.Fatal(resp.Err())
	}
	<-sub.Events()

	tests := []struct {
		name   string
		params any
		code   api.ErrorCode
	}{
		{"an existing ticket", ok, api.CodeConflict},
		{"no services", api.TicketNewParams{ID: "other", Title: "T"}, api.CodeInvalid},
		{"params that are not an object", "oops", api.CodeInvalid},
		{"no params", nil, api.CodeInvalid},
		{"an unknown source", api.TicketNewParams{From: "jira:PROJ-1", Services: ok.Services}, api.CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr *api.Error
			if err := c.call(t, api.MethodTicketNew, tt.params).Err(); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
	select {
	case ev := <-sub.Events():
		t.Errorf("a failed create published %s", ev.Type)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTicketStartHandlerErrors(t *testing.T) {
	dir := filepath.Join(testkit.RealTempDir(t), "tickets")
	mgr := state.NewManager(testkit.NewFakeClock(epoch))
	lc := lifecycle.New(lifecycle.Options{Dir: dir, States: mgr})
	h := start(t, func(s *daemon.Server) { daemon.RegisterLifecycleHandlers(s, lc) })
	c := dial(t, h.opts.SocketPath)
	c.hello(t)

	tests := []struct {
		name   string
		params any
		code   api.ErrorCode
	}{
		{"an unknown ticket", api.TicketStartParams{ID: "nope"}, api.CodeNotFound},
		{"an invalid id", api.TicketStartParams{ID: "../etc"}, api.CodeInvalid},
		{"params that are not an object", "oops", api.CodeInvalid},
		{"no params", nil, api.CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiErr *api.Error
			if err := c.call(t, api.MethodTicketStart, tt.params).Err(); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

func TestTicketStopHandlerErrors(t *testing.T) {
	dir := filepath.Join(testkit.RealTempDir(t), "tickets")
	lc := lifecycle.New(lifecycle.Options{Dir: dir, States: state.NewManager(testkit.NewFakeClock(epoch))})
	h := start(t, func(s *daemon.Server) { daemon.RegisterLifecycleHandlers(s, lc) })
	c := dial(t, h.opts.SocketPath)
	c.hello(t)
	for name, tt := range map[string]struct {
		params any
		code   api.ErrorCode
	}{
		"an unknown ticket":             {api.TicketStopParams{ID: "nope"}, api.CodeNotFound},
		"an invalid id":                 {api.TicketStopParams{ID: "../etc"}, api.CodeInvalid},
		"params that are not an object": {"oops", api.CodeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			var apiErr *api.Error
			if err := c.call(t, api.MethodTicketStop, tt.params).Err(); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

func TestGateHandlerErrors(t *testing.T) {
	dir := filepath.Join(testkit.RealTempDir(t), "tickets")
	lc := lifecycle.New(lifecycle.Options{Dir: dir, States: state.NewManager(testkit.NewFakeClock(epoch))})
	h := start(t, func(s *daemon.Server) { daemon.RegisterLifecycleHandlers(s, lc) })
	c := dial(t, h.opts.SocketPath)
	c.hello(t)
	for name, tt := range map[string]struct {
		method string
		params any
		code   api.ErrorCode
	}{
		"list on an unknown ticket":    {api.MethodGateList, api.GateListParams{ID: "nope"}, api.CodeNotFound},
		"approve on an unknown ticket": {api.MethodGateApprove, api.GateApproveParams{ID: "nope", Gate: domain.GatePlanApproval}, api.CodeNotFound},
		"list with bad params":         {api.MethodGateList, "oops", api.CodeInvalid},
		"approve with bad params":      {api.MethodGateApprove, "oops", api.CodeInvalid},
		"approve with no gate":         {api.MethodGateApprove, map[string]string{"id": "x"}, api.CodeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			var apiErr *api.Error
			if err := c.call(t, tt.method, tt.params).Err(); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

func TestPhaseHandlerErrors(t *testing.T) {
	dir := filepath.Join(testkit.RealTempDir(t), "tickets")
	lc := lifecycle.New(lifecycle.Options{Dir: dir, States: state.NewManager(testkit.NewFakeClock(epoch))})
	h := start(t, func(s *daemon.Server) { daemon.RegisterLifecycleHandlers(s, lc) })
	c := dial(t, h.opts.SocketPath)
	c.hello(t)
	for name, tt := range map[string]struct {
		method string
		params any
		code   api.ErrorCode
	}{
		"set on an unknown ticket":        {api.MethodPhaseSet, api.PhaseSetParams{ID: "nope", To: domain.PhaseImplement}, api.CodeNotFound},
		"request on an unknown ticket":    {api.MethodTransitionRequest, api.TransitionRequestParams{ID: "nope", Service: "api", To: domain.PhaseImplement}, api.CodeNotFound},
		"a request without a service":     {api.MethodTransitionRequest, api.TransitionRequestParams{ID: "nope", To: domain.PhaseImplement}, api.CodeInvalid},
		"set with bad params":             {api.MethodPhaseSet, "oops", api.CodeInvalid},
		"a request with bad params":       {api.MethodTransitionRequest, "oops", api.CodeInvalid},
		"set with an unknown phase value": {api.MethodPhaseSet, map[string]string{"id": "x", "to": "daydream"}, api.CodeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			var apiErr *api.Error
			if err := c.call(t, tt.method, tt.params).Err(); !errors.As(err, &apiErr) || apiErr.Code != tt.code {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}
