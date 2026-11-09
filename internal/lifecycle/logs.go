package lifecycle

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
)

// logSource is one container whose output can be shown, under the name the user knows
// it by.
type logSource struct {
	name, container string
}

// logSources lists a ticket's containers: its services, dependencies, and (in internal
// mode) the egress proxy. Agent containers only idle, so they have no output to show.
func logSources(t domain.Ticket) []logSource {
	var out []logSource
	for _, svc := range t.Services {
		if runsInDocker(svc) {
			out = append(out, logSource{svc.Name, ContainerName(t.ID, svc.Name)})
		}
	}
	for _, dep := range t.Dependencies {
		out = append(out, logSource{dep.Name, DependencyName(t.ID, dep.Name)})
	}
	return append(out, logSource{"proxy", ProxyName(t.ID)})
}

// StreamLogs sends the ticket's container logs, each line tagged with its container's
// name. Containers that have exited still have their logs. Without Follow the
// containers are shown one after another; with it, lines are interleaved as they
// arrive until ctx ends.
func (m *Manager) StreamLogs(ctx context.Context, p api.LogsParams, send func(any) error) error {
	if m.opts.Docker == nil {
		return &api.Error{Code: api.CodeUnavailable, Message: "container logs need Docker, which this daemon was started without"}
	}
	if err := domain.ValidateTicketID(p.Ticket); err != nil {
		return invalid("%v", err)
	}
	dir := filepath.Join(m.opts.Dir, p.Ticket)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q not found", p.Ticket)}
	}
	t, err := config.LoadTicket(filepath.Join(dir, "ticket.yaml"), config.TicketOptions{RepoCheck: func(string) error { return nil }})
	if err != nil {
		return invalid("%v", err)
	}
	if p.Tail < 0 {
		return invalid("tail must be 0 (everything) or more")
	}

	var sources []logSource
	for _, s := range logSources(t) {
		if p.Service == "" || p.Service == s.name {
			sources = append(sources, s)
		}
	}
	if len(sources) == 0 {
		names := make([]string, 0)
		for _, s := range logSources(t) {
			names = append(names, s.name)
		}
		sort.Strings(names)
		return &api.Error{Code: api.CodeNotFound, Message: fmt.Sprintf("ticket %q has no %q to show logs for (try one of: %s)", p.Ticket, p.Service, strings.Join(names, ", "))}
	}

	var readers []namedReader
	for _, s := range sources {
		r, err := m.opts.Docker.Logs(ctx, s.container, docker.LogOptions{Tail: p.Tail, Follow: p.Follow})
		switch {
		case errors.Is(err, docker.ErrNotFound) && p.Service == "":
			continue // not started (or no proxy in bridge mode): nothing to show
		case errors.Is(err, docker.ErrNotFound):
			return &api.Error{Code: api.CodeNotRunning, Message: fmt.Sprintf("%q has no container yet; start the ticket first", s.name)}
		case err != nil:
			for _, r := range readers {
				r.r.Close()
			}
			return dockerError(fmt.Sprintf("ticket %q: logs of %s", p.Ticket, s.name), err)
		}
		readers = append(readers, namedReader{s.name, r})
	}

	if !p.Follow {
		for _, nr := range readers {
			err := scanLines(nr, func(line string) error { return send(api.LogLine{Service: nr.name, Line: line}) })
			nr.r.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}

	// Following: one goroutine per container, lines sent in the order they arrive.
	lines := make(chan api.LogLine)
	var wg sync.WaitGroup
	followCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, nr := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer nr.r.Close()
			scanLines(nr, func(line string) error {
				select {
				case lines <- api.LogLine{Service: nr.name, Line: line}:
					return nil
				case <-followCtx.Done():
					return followCtx.Err()
				}
			})
		}()
	}
	go func() { wg.Wait(); close(lines) }()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return nil // every container has stopped
			}
			if err := send(l); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type namedReader struct {
	name string
	r    interface {
		Read([]byte) (int, error)
		Close() error
	}
}

func scanLines(nr namedReader, each func(string) error) error {
	sc := bufio.NewScanner(nr.r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if err := each(sc.Text()); err != nil {
			return err
		}
	}
	return nil
}
