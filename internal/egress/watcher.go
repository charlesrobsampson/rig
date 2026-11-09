package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/events"
)

// Watcher turns what the proxies refuse into `egress.denied` events, so the dashboard
// can show "an agent tried to reach X" (useful for finding a missing allowlist entry).
// It finds proxy containers by their rig.role label and follows their JSON logs.
type Watcher struct {
	Docker   docker.Docker
	Bus      *events.Bus
	Clock    clock.Clock   // nil means the real clock
	Interval time.Duration // how often it looks for new proxies (default 2s)
	Logger   *slog.Logger

	mu        sync.Mutex
	following map[string]bool      // container ids being followed
	last      map[string]time.Time // newest entry already published, per container
	wg        sync.WaitGroup
}

// Run watches until ctx is cancelled and every follower has stopped. Entries older than
// the moment Run began are history and are not announced.
func (w *Watcher) Run(ctx context.Context) {
	if w.Clock == nil {
		w.Clock = clock.Real()
	}
	if w.Interval == 0 {
		w.Interval = 2 * time.Second
	}
	if w.Logger == nil {
		w.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	w.mu.Lock()
	w.following, w.last = map[string]bool{}, map[string]time.Time{}
	w.mu.Unlock()
	began := w.Clock.Now()

	for {
		w.scan(ctx, began)
		timer := w.Clock.NewTimer(w.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			w.wg.Wait()
			return
		case <-timer.C():
		}
	}
}

func (w *Watcher) scan(ctx context.Context, began time.Time) {
	list, err := w.Docker.ListContainers(ctx, map[string]string{docker.LabelRole: docker.RoleProxy})
	if err != nil {
		return // Docker may be stopped; try again next time
	}
	for _, c := range list {
		if c.State != docker.StateRunning {
			continue
		}
		w.mu.Lock()
		busy := w.following[c.ID]
		w.following[c.ID] = true
		w.mu.Unlock()
		if busy {
			continue
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.follow(ctx, c, began)
			w.mu.Lock()
			delete(w.following, c.ID)
			w.mu.Unlock()
		}()
	}
}

func (w *Watcher) follow(ctx context.Context, c docker.ContainerInfo, began time.Time) {
	r, err := w.Docker.Logs(ctx, c.ID, docker.LogOptions{Follow: true})
	if err != nil {
		return
	}
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Action != "deny" {
			continue
		}
		w.mu.Lock()
		stale := !e.TS.After(began) || !e.TS.After(w.last[c.ID])
		if !stale {
			w.last[c.ID] = e.TS
		}
		w.mu.Unlock()
		if stale {
			continue
		}
		host, portText, err := net.SplitHostPort(e.Host)
		if err != nil {
			host, portText = e.Host, "0"
		}
		port, _ := strconv.Atoi(portText)
		if err := w.Bus.Publish(api.EventEgressDenied, c.Labels[docker.LabelTicket], "", api.EgressDeniedData{Host: host, Port: port, Reason: e.Reason}); err != nil {
			w.Logger.Warn("publish egress.denied", "err", err)
		}
	}
}
