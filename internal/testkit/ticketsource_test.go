package testkit_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/charlesrobsampson/rig/internal/testkit"
	"github.com/charlesrobsampson/rig/internal/ticket"
)

func TestFakeTicketSource(t *testing.T) {
	src := testkit.NewFakeTicketSource()
	src.Add("PROJ-1", ticket.Seed{ID: "proj-1", Title: "First", Description: "d", URL: "https://example.test/1"})

	got, err := src.Fetch(context.Background(), "PROJ-1")
	if err != nil || got.Title != "First" || got.URL != "https://example.test/1" {
		t.Fatalf("Fetch = %+v, %v", got, err)
	}
	if _, err := src.Fetch(context.Background(), "PROJ-404"); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("unknown reference = %v, want ErrNotFound", err)
	}
	if want := []string{"PROJ-1", "PROJ-404"}; !reflect.DeepEqual(src.Fetched(), want) {
		t.Errorf("Fetched = %v, want %v", src.Fetched(), want)
	}

	src.FailWith(errors.New("backend down"))
	if _, err := src.Fetch(context.Background(), "PROJ-1"); err == nil || err.Error() != "backend down" {
		t.Errorf("after FailWith, Fetch = %v", err)
	}
}
