package testkit

import (
	"context"
	"fmt"
	"sync"

	"github.com/charlesrobsampson/rig/internal/ticket"
)

// FakeTicketSource is an in-memory ticket.Source for tests: seed it with Add, make it
// fail with FailWith, and see what was asked for with Fetched.
type FakeTicketSource struct {
	mu      sync.Mutex
	seeds   map[string]ticket.Seed
	fetched []string
	err     error
}

var _ ticket.Source = (*FakeTicketSource)(nil)

func NewFakeTicketSource() *FakeTicketSource {
	return &FakeTicketSource{seeds: map[string]ticket.Seed{}}
}

func (f *FakeTicketSource) Add(ref string, seed ticket.Seed) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeds[ref] = seed
}

// FailWith makes every later Fetch return err (nil restores normal behavior).
func (f *FakeTicketSource) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *FakeTicketSource) Fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...)
}

func (f *FakeTicketSource) Fetch(_ context.Context, ref string) (ticket.Seed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, ref)
	if f.err != nil {
		return ticket.Seed{}, f.err
	}
	seed, ok := f.seeds[ref]
	if !ok {
		return ticket.Seed{}, fmt.Errorf("%w: %s", ticket.ErrNotFound, ref)
	}
	return seed, nil
}
