// Package reconcile compares what state.json says exists with what really does: the
// checkouts, containers, networks and terminal panes. It marks every difference as drift
// and never guesses a repair; the one thing it can remove, on request, is an orphan, a
// rig-labelled Docker container or network that belongs to nothing (docs/formats.md §7).
package reconcile

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/clock"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/events"
	"github.com/charlesrobsampson/rig/internal/state"
	"github.com/charlesrobsampson/rig/internal/terminal"
)

// Drift kinds (api.DriftItem.Kind).
const (
	CheckoutMissing            = "checkout_missing"
	ServiceContainerMissing    = "service_container_missing"
	DependencyContainerMissing = "dependency_container_missing"
	AgentContainerMissing      = "agent_container_missing"
	AgentPaneMissing           = "agent_pane_missing"
	NetworkMissing             = "network_missing"
	OrphanContainer            = "orphan_container"
	OrphanNetwork              = "orphan_network"
)

type Options struct {
	Dir     string // the tickets directory
	States  *state.Manager
	Docker  docker.Docker    // nil: containers and networks are not checked
	Backend terminal.Backend // nil: panes are not checked
	Bus     *events.Bus      // nil: no events
	Clock   clock.Clock      // nil means the real clock
	Logger  *slog.Logger
	// Busy says a ticket has an operation under way (a start or stop): it is left alone,
	// because what exists is not yet, or no longer, what state.json says.
	Busy func(ticket string) bool
	// Interval is how often Run repeats the check in the background (default 1 minute).
	Interval time.Duration
}

type Reconciler struct {
	o Options

	mu    sync.Mutex // serializes passes (a fix must not race a check)
	last  map[string][]string
	lastK string
}

func New(o Options) *Reconciler {
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Interval == 0 {
		o.Interval = time.Minute
	}
	return &Reconciler{o: o, last: map[string][]string{}}
}

// Drift is the last pass's findings for one ticket, one line each. Nothing is checked
// here, so it is cheap enough for every ticket.list.
func (r *Reconciler) Drift(ticket string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.last[ticket]...)
}

// Run checks at once, then every interval, until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		if _, err := r.Reconcile(ctx, false); err != nil && ctx.Err() == nil {
			r.o.Logger.Warn("reconcile failed", "err", err)
		}
		t := r.o.Clock.NewTimer(r.o.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
	}
}

// facts is what is known about one ticket directory.
type facts struct {
	ticket domain.Ticket
	state  state.State
	loaded bool // ticket.yaml and state.json both read
}

// Reconcile compares state with reality. With fix it removes orphans afterwards.
func (r *Reconciler) Reconcile(ctx context.Context, fix bool) (api.ReconcileResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := api.ReconcileResult{Drift: []api.DriftItem{}}

	tickets := r.readTickets()
	busy := func(id string) bool { return r.o.Busy != nil && r.o.Busy(id) }

	var containers []docker.ContainerInfo
	var networks []docker.NetworkInfo
	dockerOK := false
	if r.o.Docker != nil {
		var cerr, nerr error
		containers, cerr = r.o.Docker.ListContainers(ctx, map[string]string{docker.LabelTicket: ""})
		networks, nerr = r.o.Docker.ListNetworks(ctx, map[string]string{docker.LabelTicket: ""})
		if cerr != nil || nerr != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("Docker could not be checked (%v); containers and networks are not compared", firstErr(cerr, nerr)))
		} else {
			dockerOK = true
		}
	}
	var panes map[string]terminal.Pane
	if r.o.Backend != nil {
		list, err := r.o.Backend.List(ctx)
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("the terminal backend could not be checked (%v); agent panes are not compared", err))
		} else {
			panes = make(map[string]terminal.Pane, len(list))
			for _, p := range list {
				panes[string(p.ID)] = p
			}
		}
	}

	byID, byName := map[string]docker.ContainerInfo{}, map[string]docker.ContainerInfo{}
	for _, c := range containers {
		byID[c.ID], byName[c.Name] = c, c
	}
	netByName := map[string]bool{}
	for _, n := range networks {
		netByName[n.Name] = true
	}
	findContainer := func(id string) (docker.ContainerInfo, bool) {
		if c, ok := byID[id]; ok {
			return c, true
		}
		if c, ok := byName[id]; ok {
			return c, true
		}
		for _, c := range containers { // ids may be recorded in full or abbreviated
			if strings.HasPrefix(c.ID, id) || strings.HasPrefix(id, c.ID) {
				return c, true
			}
		}
		return docker.ContainerInfo{}, false
	}

	ids := make([]string, 0, len(tickets))
	for id := range tickets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := tickets[id]
		if !f.loaded || busy(id) {
			continue
		}
		add := func(service, kind, detail string, fixable bool) {
			res.Drift = append(res.Drift, api.DriftItem{Ticket: id, Service: service, Kind: kind, Detail: detail, Fixable: fixable})
		}
		for _, svc := range f.ticket.Services {
			ss := f.state.Services[svc.Name]
			live := ss.Status == domain.ServiceProvisioned || ss.Status == domain.ServiceRunning || ss.Status == domain.ServiceError
			if live && ss.Checkout.Path != "" {
				if _, err := os.Stat(ss.Checkout.Path); err != nil {
					add(svc.Name, CheckoutMissing, fmt.Sprintf("the checkout %s is gone", ss.Checkout.Path), false)
				}
			}
			if dockerOK && ss.Status == domain.ServiceRunning && ss.ContainerID != "" {
				if problem := containerProblem(findContainer, ss.ContainerID); problem != "" {
					add(svc.Name, ServiceContainerMissing, "the service's container "+problem, false)
				}
			}
			if dockerOK && ss.Agent.ContainerID != "" {
				if problem := containerProblem(findContainer, ss.Agent.ContainerID); problem != "" {
					add(svc.Name, AgentContainerMissing, "the agent's container "+problem, false)
				}
			}
			if panes != nil && ss.Agent.PaneID != "" {
				if p, ok := panes[ss.Agent.PaneID]; !ok {
					add(svc.Name, AgentPaneMissing, fmt.Sprintf("the agent's terminal pane %s is gone", ss.Agent.PaneID), false)
				} else if !p.Alive {
					add(svc.Name, AgentPaneMissing, fmt.Sprintf("the agent's terminal pane %s has ended", ss.Agent.PaneID), false)
				}
			}
		}
		if dockerOK {
			for name, ds := range f.state.Dependencies {
				if ds.ContainerID != "" && ds.Status == domain.ServiceRunning {
					if problem := containerProblem(findContainer, ds.ContainerID); problem != "" {
						add(name, DependencyContainerMissing, "the dependency's container "+problem, false)
					}
				}
			}
			for _, n := range []*state.Network{&f.state.Network, f.state.EgressNetwork} {
				if n != nil && n.Name != "" && !netByName[n.Name] {
					add("", NetworkMissing, fmt.Sprintf("the network %s is gone", n.Name), false)
				}
			}
		}
	}

	// orphans: rig-labelled objects that belong to nothing
	if dockerOK {
		for _, c := range containers {
			id := c.Labels[docker.LabelTicket]
			if busy(id) {
				continue
			}
			if why := orphanReason(tickets, id, c.Labels); why != "" {
				res.Drift = append(res.Drift, api.DriftItem{Ticket: id, Service: c.Labels[docker.LabelService], Kind: OrphanContainer, Detail: fmt.Sprintf("container %s: %s", c.Name, why), Fixable: true})
			}
		}
		for _, n := range networks {
			id := n.Labels[docker.LabelTicket]
			if busy(id) {
				continue
			}
			if why := orphanReason(tickets, id, n.Labels); why != "" {
				res.Drift = append(res.Drift, api.DriftItem{Ticket: id, Kind: OrphanNetwork, Detail: fmt.Sprintf("network %s: %s", n.Name, why), Fixable: true})
			}
		}
	}

	if fix && dockerOK {
		r.removeOrphans(ctx, &res, containers, networks)
	}
	r.remember(res)
	return res, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// containerProblem says what is wrong with a recorded container, or "" if it is running.
func containerProblem(find func(string) (docker.ContainerInfo, bool), id string) string {
	c, ok := find(id)
	switch {
	case !ok:
		return "is gone"
	case c.State != docker.StateRunning:
		return fmt.Sprintf("%s is %s", c.Name, c.State)
	}
	return ""
}

// orphanReason is why a rig-labelled object belongs to nothing, or "" if it belongs to a
// ticket that is using it. Anything unclear is not an orphan: removing a live ticket's
// container is much worse than leaving a stray one for a human to see.
func orphanReason(tickets map[string]facts, ticket string, labels map[string]string) string {
	f, exists := tickets[ticket]
	if !exists {
		return fmt.Sprintf("no ticket %q exists", ticket)
	}
	if !f.loaded {
		return "" // the ticket's own files cannot be read: do not guess
	}
	switch labels[docker.LabelRole] {
	case docker.RoleService, docker.RoleAgent:
		if svc := labels[docker.LabelService]; svc != "" && !hasService(f.ticket, svc) {
			return fmt.Sprintf("ticket %q has no service %q", ticket, svc)
		}
	case docker.RoleDependency:
		if dep := labels[docker.LabelService]; dep != "" && !hasDependency(f.ticket, dep) {
			return fmt.Sprintf("ticket %q has no dependency %q", ticket, dep)
		}
	}
	if !f.state.InUse() {
		return fmt.Sprintf("ticket %q is stopped", ticket)
	}
	return ""
}

func hasService(t domain.Ticket, name string) bool {
	for _, s := range t.Services {
		if s.Name == name {
			return true
		}
	}
	return false
}

func hasDependency(t domain.Ticket, name string) bool {
	for _, d := range t.Dependencies {
		if d.Name == name {
			return true
		}
	}
	return false
}

// removeOrphans removes the fixable items: containers first (force: they are leftovers),
// then networks. Only objects found by the pass, carrying rig's labels, are touched.
func (r *Reconciler) removeOrphans(ctx context.Context, res *api.ReconcileResult, containers []docker.ContainerInfo, networks []docker.NetworkInfo) {
	idOfContainer := map[string]string{}
	for _, c := range containers {
		idOfContainer["container "+c.Name] = c.ID
	}
	idOfNetwork := map[string]string{}
	for _, n := range networks {
		idOfNetwork["network "+n.Name] = n.ID
	}
	var removedKeys = map[string]bool{}
	for _, kind := range []string{OrphanContainer, OrphanNetwork} {
		for _, d := range res.Drift {
			if d.Kind != kind || !d.Fixable {
				continue
			}
			key := strings.SplitN(d.Detail, ":", 2)[0] // "container NAME" / "network NAME"
			var err error
			if kind == OrphanContainer {
				err = r.o.Docker.RemoveContainer(ctx, idOfContainer[key], true)
			} else {
				err = r.o.Docker.RemoveNetwork(ctx, idOfNetwork[key])
			}
			switch {
			case err == nil || isNotFound(err):
				res.Fixed = append(res.Fixed, key)
				removedKeys[d.Ticket+"|"+key] = true
			default:
				res.Failures = append(res.Failures, fmt.Sprintf("remove %s: %v", key, err))
			}
		}
	}
	// what was removed is no longer drift
	kept := res.Drift[:0]
	for _, d := range res.Drift {
		if d.Fixable && removedKeys[d.Ticket+"|"+strings.SplitN(d.Detail, ":", 2)[0]] {
			continue
		}
		kept = append(kept, d)
	}
	res.Drift = kept
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), docker.ErrNotFound.Error())
}

// readTickets reads every ticket directory. A directory whose files cannot be read still
// counts as a ticket that exists (loaded=false), so its objects are never called orphans.
func (r *Reconciler) readTickets() map[string]facts {
	out := map[string]facts{}
	entries, err := os.ReadDir(r.o.Dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(r.o.Dir, e.Name())
		f := facts{}
		t, terr := config.LoadTicket(filepath.Join(dir, "ticket.yaml"), config.TicketOptions{RepoCheck: func(string) error { return nil }})
		st, serr := r.o.States.Load(filepath.Join(dir, "state.json"))
		if terr == nil && serr == nil {
			f = facts{ticket: t, state: st, loaded: true}
		}
		out[e.Name()] = f
	}
	return out
}

// remember keeps the findings for Drift and announces a change.
func (r *Reconciler) remember(res api.ReconcileResult) {
	next := map[string][]string{}
	var key []string
	for _, d := range res.Drift {
		line := d.Detail
		if d.Service != "" {
			line = d.Service + ": " + line
		}
		next[d.Ticket] = append(next[d.Ticket], line)
		key = append(key, d.Ticket+"|"+d.Kind+"|"+d.Detail)
	}
	sort.Strings(key)
	k := strings.Join(key, "\n")
	changed := k != r.lastK || len(res.Fixed) > 0
	r.last, r.lastK = next, k
	if changed && r.o.Bus != nil {
		if err := r.o.Bus.Publish(api.EventReconciled, "", "", api.ReconciledData{Drift: len(res.Drift), Fixed: len(res.Fixed)}); err != nil {
			r.o.Logger.Warn("reconcile: cannot publish", "err", err)
		}
	}
}
