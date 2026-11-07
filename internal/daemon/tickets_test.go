package daemon_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/daemon"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/registry"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/testkit"
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
