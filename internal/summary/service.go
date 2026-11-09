package summary

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/store"
)

// Inputs gathers what a ticket's summary is made from. The daemon's implementation reads
// the ticket's plan and tasks and the diff of its checkouts.
type Inputs interface {
	Gather(ctx context.Context, ticket string) (Input, error)
}

// Options configure the background job.
type Options struct {
	Dir        string // the tickets directory
	Store      store.Store
	Summarizer Summarizer
	Inputs     Inputs
	Bus        *events.Bus
	Clock      clock.Clock // nil means the real clock
	Logger     *slog.Logger
	// Interval is how often the job looks at the tickets (default 30s).
	Interval time.Duration
	// RequestTimeout bounds one summary (default 3 minutes).
	RequestTimeout time.Duration
	// MinBackoff and MaxBackoff bound the wait before retrying a summary that failed
	// (defaults 30s and 30m); the wait doubles with each failure in a row.
	MinBackoff, MaxBackoff time.Duration
}

// Service is the background job: it keeps each ticket's summary current without ever
// getting in the way.
type Service struct{ o Options }

func NewService(o Options) *Service {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Interval == 0 {
		o.Interval = 30 * time.Second
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 3 * time.Minute
	}
	if o.MinBackoff == 0 {
		o.MinBackoff = 30 * time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 30 * time.Minute
	}
	if o.Summarizer == nil {
		o.Summarizer = None{}
	}
	return &Service{o: o}
}

// SetInputs says where to read tickets' content from. The daemon calls it once, before
// Run, because the source needs objects built after the service itself.
func (s *Service) SetInputs(in Inputs) { s.o.Inputs = in }

// Enabled reports whether any summaries are made at all.
func (s *Service) Enabled() bool { return s.o.Summarizer.Name() != "none" }

// record is what is kept per ticket (in the runtime store, never in the ticket's files).
type record struct {
	InputHash string    `json:"input_hash,omitempty"` // of the content Text was made from
	Text      string    `json:"text,omitempty"`
	Backend   string    `json:"backend"`
	Model     string    `json:"model,omitempty"`
	Remote    bool      `json:"remote"`
	At        time.Time `json:"at,omitempty"`
	Stats     Stats     `json:"stats"`

	// the last failure: its content hash, when to try again, and how many in a row
	Error      string    `json:"error,omitempty"`
	FailedHash string    `json:"failed_hash,omitempty"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
	Failures   int       `json:"failures,omitempty"`
}

// The runtime store holds one JSON object per ticket id; the summary is its "summary"
// field, and every other field is left as it is.
const field = "summary"

func (s *Service) load(ticket string) (record, bool) {
	raw, err := s.o.Store.Get(ticket)
	if err != nil {
		return record{}, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj[field] == nil {
		return record{}, false
	}
	var r record
	if json.Unmarshal(obj[field], &r) != nil {
		return record{}, false
	}
	return r, true
}

func (s *Service) save(ticket string, r record) error {
	enc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.o.Store.Update(func(tx store.Tx) error {
		obj := map[string]json.RawMessage{}
		if raw, err := tx.Get(ticket); err == nil {
			if json.Unmarshal(raw, &obj) != nil || obj == nil {
				obj = map[string]json.RawMessage{}
			}
		}
		obj[field] = enc
		merged, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		return tx.Put(ticket, merged)
	})
}

// Info is the summary to show for a ticket, or nil when there is none to show (the
// provider is none, or nothing was ever made or failed).
func (s *Service) Info(ticket string) *api.SummaryInfo {
	if !s.Enabled() || s.o.Store == nil {
		return nil
	}
	r, ok := s.load(ticket)
	if !ok || (r.Text == "" && r.Error == "") {
		return nil
	}
	info := &api.SummaryInfo{Text: r.Text, Backend: r.Backend, Model: r.Model, Remote: r.Remote, Files: r.Stats.Files, Added: r.Stats.Added, Deleted: r.Stats.Deleted, Error: r.Error}
	if !r.At.IsZero() {
		at := r.At
		info.At = &at
	}
	return info
}

// Run looks at the tickets every interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	for {
		s.Poll(ctx)
		t := s.o.Clock.NewTimer(s.o.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
	}
}

// Poll brings every ticket's summary up to date, one request at a time. A ticket whose
// content is unchanged since its summary was made is not asked about again; one whose
// last attempt failed waits out its backoff. Nothing here returns an error: a failure is
// recorded beside the previous summary and the job carries on.
func (s *Service) Poll(ctx context.Context) {
	if !s.Enabled() || s.o.Store == nil || s.o.Inputs == nil {
		return
	}
	entries, err := os.ReadDir(s.o.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			if _, err := os.Stat(filepath.Join(s.o.Dir, e.Name(), "ticket.yaml")); err == nil {
				s.update(ctx, e.Name())
			}
		}
	}
}

func (s *Service) update(ctx context.Context, ticket string) {
	in, err := s.o.Inputs.Gather(ctx, ticket)
	if err != nil {
		s.o.Logger.Debug("summary: cannot gather", "ticket", ticket, "err", err)
		return
	}
	if in.Empty() {
		return
	}
	hash := in.Hash()
	now := s.o.Clock.Now()
	prev, had := s.load(ticket)
	switch {
	case had && prev.InputHash == hash && prev.Text != "":
		return // unchanged since the last summary
	case had && prev.FailedHash == hash && now.Before(prev.RetryAfter):
		return // failed on exactly this content and still backing off
	}

	rctx, cancel := context.WithTimeout(ctx, s.o.RequestTimeout)
	defer cancel()
	text, err := s.o.Summarizer.Summarize(rctx, in)
	if ctx.Err() != nil {
		return // shutting down is not a failure
	}
	if err == nil && strings.TrimSpace(text) == "" {
		err = errors.New("the summarizer returned nothing")
	}

	if err != nil {
		prev.Backend, prev.Model, prev.Remote = s.o.Summarizer.Name(), s.o.Summarizer.Model(), s.o.Summarizer.Remote()
		prev.Error, prev.FailedHash = err.Error(), hash
		prev.Failures++
		prev.RetryAfter = now.Add(s.backoff(prev.Failures))
		s.o.Logger.Warn("summary failed", "ticket", ticket, "backend", prev.Backend, "err", err, "retry_at", prev.RetryAfter)
		s.store(ticket, prev, err.Error())
		return
	}
	rec := record{
		InputHash: hash, Text: strings.TrimSpace(text), At: now, Stats: in.Total(),
		Backend: s.o.Summarizer.Name(), Model: s.o.Summarizer.Model(), Remote: s.o.Summarizer.Remote(),
	}
	s.store(ticket, rec, "")
}

// backoff doubles from MinBackoff with each failure in a row, up to MaxBackoff.
func (s *Service) backoff(failures int) time.Duration {
	d := s.o.MinBackoff
	for i := 1; i < failures && d < s.o.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, s.o.MaxBackoff)
}

func (s *Service) store(ticket string, r record, failure string) {
	if err := s.save(ticket, r); err != nil {
		s.o.Logger.Warn("summary: cannot save", "ticket", ticket, "err", err)
		return
	}
	if s.o.Bus != nil {
		if err := s.o.Bus.Publish(api.EventSummaryUpdated, ticket, "", api.SummaryUpdatedData{Backend: r.Backend, Error: failure}); err != nil {
			s.o.Logger.Warn("summary: cannot publish", "err", err)
		}
	}
}
