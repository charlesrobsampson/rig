// Package contractwatch keeps an eye on every multi-service ticket's contract.md. It
// notices new proposals (and announces each exactly once), keeps the "contract change
// pending" flag in state.json right, and flags an edit of the agreed section that did
// not come through rig once the plan is approved. It polls the files on the injected
// clock and waits for them to be quiet before reading, so a burst of writes is one
// event.
package contractwatch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/contract"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/state"
)

type Options struct {
	Dir      string // the tickets directory
	States   *state.Manager
	Bus      *events.Bus
	Clock    clock.Clock // nil means the real clock
	Logger   *slog.Logger
	Interval time.Duration // between looks at the files (default 1s)
	Debounce time.Duration // how long a file must stay unchanged before it is read (default 500ms)
}

type Watcher struct {
	opts    Options
	tickets map[string]*watched
}

type watched struct {
	sig       signature
	changedAt time.Time
	processed signature
	statuses  map[string]contract.Status // proposal id -> status as last read
	read      bool
}

type signature struct {
	mod  time.Time
	size int64
}

func New(o Options) *Watcher {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Interval == 0 {
		o.Interval = time.Second
	}
	if o.Debounce == 0 {
		o.Debounce = 500 * time.Millisecond
	}
	return &Watcher{opts: o, tickets: map[string]*watched{}}
}

// Run looks at the contracts until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	for {
		w.Poll(ctx)
		timer := w.opts.Clock.NewTimer(w.opts.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
	}
}

// Poll looks at every ticket's contract once, reading those that changed and have been
// quiet for the debounce time.
func (w *Watcher) Poll(ctx context.Context) {
	entries, err := os.ReadDir(w.opts.Dir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	now := w.opts.Clock.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(w.opts.Dir, e.Name(), "contract.md")
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		seen[e.Name()] = true
		t := w.tickets[e.Name()]
		sig := signature{info.ModTime(), info.Size()}
		if t == nil {
			// first sight: read at once; there is nothing to wait for
			t = &watched{sig: sig, changedAt: now.Add(-w.opts.Debounce), statuses: map[string]contract.Status{}}
			w.tickets[e.Name()] = t
		} else if sig != t.sig {
			t.sig, t.changedAt = sig, now
		}
		if t.sig != t.processed && now.Sub(t.changedAt) >= w.opts.Debounce {
			t.processed = t.sig
			w.process(e.Name(), path, t)
		}
	}
	for id := range w.tickets {
		if !seen[id] {
			delete(w.tickets, id) // the ticket or its contract is gone
		}
	}
}

func (w *Watcher) process(id, path string, t *watched) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	doc := contract.Parse(string(raw))
	statePath := filepath.Join(w.opts.Dir, id, "state.json")

	var announce []string
	var violation string
	var resolved []api.ContractResolvedData
	err = w.opts.States.Update(statePath, func(st *state.State) error {
		announce, violation, resolved = nil, "", nil
		before := st.Contract
		open := doc.OpenProposals()
		for _, p := range doc.Proposals {
			if !slices.Contains(st.Contract.Seen, p.ID) {
				st.Contract.Seen = append(st.Contract.Seen, p.ID)
				if p.Status == contract.Open {
					announce = append(announce, p.ID)
				}
			}
			if prev, known := t.statuses[p.ID]; known && prev == contract.Open && p.Status != contract.Open {
				resolved = append(resolved, api.ContractResolvedData{Proposal: p.ID, Outcome: string(p.Status)})
			}
		}
		st.Contract.Pending = len(open) > 0

		hash := doc.AgreedHash()
		switch {
		case st.Contract.AgreedHash == "" || st.Phase == domain.PhasePlan:
			// nothing was agreed yet, or the plan is still being drafted: the agreed
			// section is allowed to change, and what it says now is the baseline
			st.Contract.AgreedHash = hash
		case hash != st.Contract.AgreedHash:
			violation = "the agreed section of contract.md was edited directly instead of through a proposal; review the change, then re-agree or revert it"
			st.Contract.Violation = violation
			st.Contract.AgreedHash = hash
		}
		if st.Contract.Pending == before.Pending && st.Contract.AgreedHash == before.AgreedHash && st.Contract.Violation == before.Violation && len(st.Contract.Seen) == len(before.Seen) {
			return errUnchanged
		}
		return nil
	})
	if err != nil && err != errUnchanged {
		w.opts.Logger.Warn("update contract state", "ticket", id, "err", err)
		return
	}

	for _, p := range doc.Proposals {
		t.statuses[p.ID] = p.Status
	}
	t.read = true
	for _, pid := range announce {
		w.publish(api.EventContractPending, id, api.ContractPendingData{Proposal: pid})
	}
	for _, r := range resolved {
		w.publish(api.EventContractResolved, id, r)
	}
	if violation != "" {
		w.publish(api.EventContractViolation, id, api.ContractViolationData{Detail: violation})
	}
}

var errUnchanged = fmt.Errorf("unchanged")

func (w *Watcher) publish(typ api.EventType, ticket string, data any) {
	if w.opts.Bus != nil {
		if err := w.opts.Bus.Publish(typ, ticket, "", data); err != nil {
			w.opts.Logger.Warn("publish contract event", "type", typ, "err", err)
		}
	}
}
