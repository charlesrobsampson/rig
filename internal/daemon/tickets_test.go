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
