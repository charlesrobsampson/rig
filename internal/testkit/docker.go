package testkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charlesrobsampson/rig/internal/docker"
)

// FakeDocker is an in-memory docker.Docker. It runs nothing: containers are records
// whose output, exit and health a test scripts, but everything Docker enforces is
// enforced here too: unique network and container names, networks that refuse removal
// while containers are attached, host ports that only one running container can hold,
// peers that resolve each other by name on a shared network, and logs that outlive the
// container's process.
type FakeDocker struct {
	mu       sync.Mutex
	down     error
	nets     map[string]*fakeNet // by id
	ctrs     map[string]*fakeCtr // by id
	images   map[string]bool
	nextNet  int
	nextCtr  int
	nextPort int
	changed  chan struct{} // closed and replaced whenever a container changes or logs

	execFn func(id string, spec docker.ExecSpec) (docker.ExecResult, error)
	execs  map[string][]docker.ExecSpec

	healthImages map[string]bool
	builds       map[string]int
	lastBuild    docker.BuildSpec
	buildHook    func(docker.BuildSpec) error
	buildLog     []docker.BuildSpec
}

var _ docker.Docker = (*FakeDocker)(nil)

type fakeNet struct {
	info docker.NetworkInfo
	num  int
}

type fakeCtr struct {
	info docker.ContainerInfo
	spec docker.ContainerSpec
	logs []string
	ip   map[string]string // per network name
}

func NewFakeDocker() *FakeDocker {
	return &FakeDocker{
		nets: map[string]*fakeNet{}, ctrs: map[string]*fakeCtr{}, images: map[string]bool{},
		nextPort: 40000, changed: make(chan struct{}), execs: map[string][]docker.ExecSpec{}, healthImages: map[string]bool{}, builds: map[string]int{},
	}
}

// Down makes every call fail as if the daemon were unreachable; Up undoes it.
func (f *FakeDocker) Down(cause error) { f.mu.Lock(); f.down = cause; f.mu.Unlock() }
func (f *FakeDocker) Up()              { f.Down(nil) }

// AddImage says an image exists locally (a base image that would be pulled).
func (f *FakeDocker) AddImage(ref string) { f.mu.Lock(); f.images[ref] = true; f.mu.Unlock() }

// OnExec sets what Exec returns; the default is exit 0 with no output.
func (f *FakeDocker) OnExec(fn func(id string, spec docker.ExecSpec) (docker.ExecResult, error)) {
	f.mu.Lock()
	f.execFn = fn
	f.mu.Unlock()
}

// Execs lists what was run in a container, in order.
func (f *FakeDocker) Execs(nameOrID string) []docker.ExecSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(nameOrID); c != nil {
		return append([]docker.ExecSpec(nil), f.execs[c.info.ID]...)
	}
	return nil
}

// Print appends lines to a container's output, as its process would.
func (f *FakeDocker) Print(nameOrID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(nameOrID); c != nil {
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			c.logs = append(c.logs, line)
		}
		f.broadcast()
	}
}

// Exit ends a running container's process with a code; its logs stay readable.
func (f *FakeDocker) Exit(nameOrID string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(nameOrID); c != nil {
		f.stop(c, code)
	}
}

// SetHealth moves a container with a healthcheck to the given status.
func (f *FakeDocker) SetHealth(nameOrID string, h docker.Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(nameOrID); c != nil && c.info.Health != docker.HealthNone {
		c.info.Health = h
		f.broadcast()
	}
}

// Resolve answers what a lookup of name from inside a container returns: an address
// when a peer on a shared network has that name or alias, ErrNotFound otherwise.
func (f *FakeDocker) Resolve(from, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.find(from)
	if src == nil {
		return "", fmt.Errorf("%w: container %s", docker.ErrNotFound, from)
	}
	for netName := range src.info.Networks {
		for _, c := range f.ctrs {
			if c.info.State != docker.StateRunning {
				continue
			}
			for _, alias := range c.info.Networks[netName].Aliases {
				if alias == name {
					return c.ip[netName], nil
				}
			}
		}
	}
	return "", fmt.Errorf("%w: %s does not resolve from %s", docker.ErrNotFound, name, from)
}

// OnBuild runs fn inside every BuildImage call (the build context still exists then),
// and a non-nil result fails the build.
func (f *FakeDocker) OnBuild(fn func(spec docker.BuildSpec) error) {
	f.mu.Lock()
	f.buildHook = fn
	f.mu.Unlock()
}

// Builds lists every BuildImage call, in order.
func (f *FakeDocker) Builds() []docker.BuildSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.BuildSpec(nil), f.buildLog...)
}

// ContainerSpec returns the spec a container was created from (zero if unknown).
func (f *FakeDocker) ContainerSpec(nameOrID string) docker.ContainerSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(nameOrID); c != nil {
		return c.spec
	}
	return docker.ContainerSpec{}
}

// SetImageHealthcheck makes containers of the image report health (as an image with
// its own HEALTHCHECK does): starting after start, until SetHealth moves them.
func (f *FakeDocker) SetImageHealthcheck(image string) {
	f.mu.Lock()
	f.healthImages[image] = true
	f.mu.Unlock()
}

// HostPorts lists the host ports held by running containers, sorted.
func (f *FakeDocker) HostPorts() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ports []int
	for _, c := range f.ctrs {
		if c.info.State == docker.StateRunning {
			for _, p := range c.info.Ports {
				ports = append(ports, p.Host)
			}
		}
	}
	sort.Ints(ports)
	return ports
}

func (f *FakeDocker) broadcast() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *FakeDocker) unavailable() error {
	if f.down != nil {
		return fmt.Errorf("%w: %v", docker.ErrUnavailable, f.down)
	}
	return nil
}

func (f *FakeDocker) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.unavailable()
}

// begin is the common start of every call: context and availability checks, then the lock.
func (f *FakeDocker) begin(ctx context.Context) (unlock func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if err := f.unavailable(); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	return f.mu.Unlock, nil
}

func (f *FakeDocker) findNet(nameOrID string) *fakeNet {
	if n, ok := f.nets[nameOrID]; ok {
		return n
	}
	for _, n := range f.nets {
		if n.info.Name == nameOrID {
			return n
		}
	}
	return nil
}

func (f *FakeDocker) find(nameOrID string) *fakeCtr {
	if c, ok := f.ctrs[nameOrID]; ok {
		return c
	}
	for _, c := range f.ctrs {
		if c.info.Name == nameOrID {
			return c
		}
	}
	return nil
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (f *FakeDocker) netView(n *fakeNet) docker.NetworkInfo {
	info := n.info
	info.Labels = copyLabels(info.Labels)
	info.Containers = nil
	for _, c := range f.ctrs {
		if _, on := c.info.Networks[n.info.Name]; on {
			info.Containers = append(info.Containers, c.info.ID)
		}
	}
	sort.Strings(info.Containers)
	return info
}

func (f *FakeDocker) CreateNetwork(ctx context.Context, spec docker.NetworkSpec) (docker.NetworkInfo, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return docker.NetworkInfo{}, err
	}
	defer unlock()
	if spec.Name == "" {
		return docker.NetworkInfo{}, fmt.Errorf("%w: a network needs a name", docker.ErrInvalid)
	}
	if f.findNet(spec.Name) != nil {
		return docker.NetworkInfo{}, fmt.Errorf("%w: network %s", docker.ErrExists, spec.Name)
	}
	f.nextNet++
	n := &fakeNet{num: f.nextNet, info: docker.NetworkInfo{ID: fmt.Sprintf("net-%d", f.nextNet), Name: spec.Name, Internal: spec.Internal, Labels: copyLabels(spec.Labels)}}
	f.nets[n.info.ID] = n
	return f.netView(n), nil
}

func (f *FakeDocker) InspectNetwork(ctx context.Context, nameOrID string) (docker.NetworkInfo, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return docker.NetworkInfo{}, err
	}
	defer unlock()
	n := f.findNet(nameOrID)
	if n == nil {
		return docker.NetworkInfo{}, fmt.Errorf("%w: network %s", docker.ErrNotFound, nameOrID)
	}
	return f.netView(n), nil
}

func (f *FakeDocker) ListNetworks(ctx context.Context, labels map[string]string) ([]docker.NetworkInfo, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var out []docker.NetworkInfo
	for _, n := range f.nets {
		if hasLabels(n.info.Labels, labels) {
			out = append(out, f.netView(n))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *FakeDocker) RemoveNetwork(ctx context.Context, nameOrID string) error {
	unlock, err := f.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	n := f.findNet(nameOrID)
	if n == nil {
		return fmt.Errorf("%w: network %s", docker.ErrNotFound, nameOrID)
	}
	if v := f.netView(n); len(v.Containers) > 0 {
		return fmt.Errorf("%w: network %s still has %d container(s) attached", docker.ErrInUse, n.info.Name, len(v.Containers))
	}
	delete(f.nets, n.info.ID)
	return nil
}

func (f *FakeDocker) ConnectNetwork(ctx context.Context, container, network string, aliases []string) error {
	unlock, err := f.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	c := f.find(container)
	if c == nil {
		return fmt.Errorf("%w: container %s", docker.ErrNotFound, container)
	}
	n := f.findNet(network)
	if n == nil {
		return fmt.Errorf("%w: network %s", docker.ErrNotFound, network)
	}
	if _, on := c.info.Networks[n.info.Name]; on {
		return fmt.Errorf("%w: %s is already on network %s", docker.ErrExists, c.info.Name, n.info.Name)
	}
	c.info.Networks[n.info.Name] = docker.ContainerNetwork{Aliases: append([]string{c.info.Name}, aliases...)}
	c.ip[n.info.Name] = fmt.Sprintf("10.%d.0.%d", n.num, 100+len(c.ip)+f.nextCtr)
	return nil
}

func (f *FakeDocker) BuildImage(ctx context.Context, spec docker.BuildSpec) error {
	unlock, err := f.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if spec.Tag == "" || !filepath.IsAbs(spec.ContextDir) {
		return fmt.Errorf("%w: a build needs a tag and an absolute context directory", docker.ErrInvalid)
	}
	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if info, err := os.Stat(filepath.Join(spec.ContextDir, dockerfile)); err != nil || info.IsDir() {
		return fmt.Errorf("%w: no Dockerfile at %s", docker.ErrInvalid, filepath.Join(spec.ContextDir, dockerfile))
	}
	if f.buildHook != nil {
		if err := f.buildHook(spec); err != nil {
			return err
		}
	}
	f.images[spec.Tag] = true
	f.builds[spec.Tag]++
	f.lastBuild = spec
	f.buildLog = append(f.buildLog, spec)
	return nil
}

func (f *FakeDocker) ImageExists(ctx context.Context, ref string) (bool, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	return f.images[ref], nil
}

// ImageID is derived from the image's name and how many times it was (re)built, so a
// rebuilt tag has a new id like a real one.
func (f *FakeDocker) ImageID(ctx context.Context, ref string) (string, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	if !f.images[ref] {
		return "", fmt.Errorf("%w: image %s", docker.ErrNotFound, ref)
	}
	return fmt.Sprintf("sha256:fake-%s-%d", ref, f.builds[ref]), nil
}

func (f *FakeDocker) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (docker.ContainerInfo, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return docker.ContainerInfo{}, err
	}
	defer unlock()
	switch {
	case spec.Name == "" || spec.Image == "":
		return docker.ContainerInfo{}, fmt.Errorf("%w: a container needs a name and an image", docker.ErrInvalid)
	case f.find(spec.Name) != nil:
		return docker.ContainerInfo{}, fmt.Errorf("%w: container %s", docker.ErrExists, spec.Name)
	case !f.images[spec.Image]:
		return docker.ContainerInfo{}, fmt.Errorf("%w: image %s", docker.ErrNotFound, spec.Image)
	}
	for _, m := range spec.Mounts {
		if !filepath.IsAbs(m.Source) || !filepath.IsAbs(m.Target) {
			return docker.ContainerInfo{}, fmt.Errorf("%w: mount %s:%s needs absolute paths", docker.ErrInvalid, m.Source, m.Target)
		}
		if _, err := os.Stat(m.Source); err != nil {
			return docker.ContainerInfo{}, fmt.Errorf("%w: mount source %s does not exist", docker.ErrInvalid, m.Source)
		}
	}
	c := &fakeCtr{spec: spec, ip: map[string]string{}}
	f.nextCtr++
	c.info = docker.ContainerInfo{
		ID: fmt.Sprintf("ctr-%d", f.nextCtr), Name: spec.Name, Image: spec.Image, State: docker.StateCreated, Health: docker.HealthNone,
		Labels: copyLabels(spec.Labels), Env: copyLabels(spec.Env), Mounts: append([]docker.Mount(nil), spec.Mounts...),
		Ports: append([]docker.PortBinding(nil), spec.Ports...), Networks: map[string]docker.ContainerNetwork{},
	}
	if spec.Network != "" {
		n := f.findNet(spec.Network)
		if n == nil {
			return docker.ContainerInfo{}, fmt.Errorf("%w: network %s", docker.ErrNotFound, spec.Network)
		}
		aliases := append([]string{spec.Name}, spec.Aliases...)
		c.info.Networks[n.info.Name] = docker.ContainerNetwork{Aliases: aliases}
		c.ip[n.info.Name] = fmt.Sprintf("10.%d.0.%d", n.num, f.nextCtr+1)
	}
	f.ctrs[c.info.ID] = c
	return f.view(c), nil
}

func (f *FakeDocker) view(c *fakeCtr) docker.ContainerInfo {
	info := c.info
	info.Labels = copyLabels(c.info.Labels)
	info.Env = copyLabels(c.info.Env)
	info.Ports = append([]docker.PortBinding(nil), c.info.Ports...)
	info.Mounts = append([]docker.Mount(nil), c.info.Mounts...)
	info.Networks = map[string]docker.ContainerNetwork{}
	for k, v := range c.info.Networks {
		info.Networks[k] = docker.ContainerNetwork{Aliases: append([]string(nil), v.Aliases...)}
	}
	return info
}

func (f *FakeDocker) StartContainer(ctx context.Context, nameOrID string) error {
	unlock, err := f.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	c := f.find(nameOrID)
	if c == nil {
		return fmt.Errorf("%w: container %s", docker.ErrNotFound, nameOrID)
	}
	if c.info.State == docker.StateRunning {
		return nil
	}
	taken := map[int]bool{}
	for _, other := range f.ctrs {
		if other.info.State == docker.StateRunning {
			for _, p := range other.info.Ports {
				taken[p.Host] = true
			}
		}
	}
	ports := append([]docker.PortBinding(nil), c.spec.Ports...)
	for i, p := range ports {
		if p.Host == 0 {
			for taken[f.nextPort] {
				f.nextPort++
			}
			ports[i].Host = f.nextPort
			f.nextPort++
		} else if taken[p.Host] {
			return fmt.Errorf("%w: host port %d is already allocated", docker.ErrInUse, p.Host)
		}
	}
	c.info.Ports = ports
	c.info.State, c.info.ExitCode = docker.StateRunning, 0
	if c.spec.Healthcheck != nil || f.healthImages[c.spec.Image] {
		c.info.Health = docker.HealthStarting
	}
	f.broadcast()
	return nil
}

func (f *FakeDocker) stop(c *fakeCtr, code int) {
	if c.info.State != docker.StateRunning {
		return
	}
	c.info.State, c.info.ExitCode = docker.StateExited, code
	if c.spec.Healthcheck != nil || f.healthImages[c.spec.Image] {
		c.info.Health = docker.HealthNone
	}
	f.broadcast()
}

func (f *FakeDocker) StopContainer(ctx context.Context, nameOrID string, _ time.Duration) error {
	unlock, err := f.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	c := f.find(nameOrID)
	if c == nil {
		return fmt.Errorf("%w: container %s", docker.ErrNotFound, nameOrID)
	}
	f.stop(c, 143)
	return nil
}

func (f *FakeDocker) RemoveContainer(ctx context.Context, nameOrID string, force bool) error {
	unlock, err := f.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	c := f.find(nameOrID)
	if c == nil {
		return fmt.Errorf("%w: container %s", docker.ErrNotFound, nameOrID)
	}
	if c.info.State == docker.StateRunning && !force {
		return fmt.Errorf("%w: container %s is running; stop it or force the removal", docker.ErrInUse, c.info.Name)
	}
	f.stop(c, 137)
	delete(f.ctrs, c.info.ID)
	delete(f.execs, c.info.ID)
	f.broadcast()
	return nil
}

func (f *FakeDocker) InspectContainer(ctx context.Context, nameOrID string) (docker.ContainerInfo, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return docker.ContainerInfo{}, err
	}
	defer unlock()
	c := f.find(nameOrID)
	if c == nil {
		return docker.ContainerInfo{}, fmt.Errorf("%w: container %s", docker.ErrNotFound, nameOrID)
	}
	return f.view(c), nil
}

func (f *FakeDocker) ListContainers(ctx context.Context, labels map[string]string) ([]docker.ContainerInfo, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var out []docker.ContainerInfo
	for _, c := range f.ctrs {
		if hasLabels(c.info.Labels, labels) {
			out = append(out, f.view(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *FakeDocker) Exec(ctx context.Context, nameOrID string, spec docker.ExecSpec) (docker.ExecResult, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return docker.ExecResult{}, err
	}
	c := f.find(nameOrID)
	if c == nil {
		unlock()
		return docker.ExecResult{}, fmt.Errorf("%w: container %s", docker.ErrNotFound, nameOrID)
	}
	if c.info.State != docker.StateRunning {
		unlock()
		return docker.ExecResult{}, fmt.Errorf("%w: %s", docker.ErrNotRunning, c.info.Name)
	}
	if len(spec.Command) == 0 {
		unlock()
		return docker.ExecResult{}, fmt.Errorf("%w: an exec needs a command", docker.ErrInvalid)
	}
	f.execs[c.info.ID] = append(f.execs[c.info.ID], spec)
	fn, id := f.execFn, c.info.ID
	unlock()
	if fn == nil {
		return docker.ExecResult{}, nil
	}
	return fn(id, spec)
}

func (f *FakeDocker) Logs(ctx context.Context, nameOrID string, opts docker.LogOptions) (io.ReadCloser, error) {
	unlock, err := f.begin(ctx)
	if err != nil {
		return nil, err
	}
	c := f.find(nameOrID)
	if c == nil {
		unlock()
		return nil, fmt.Errorf("%w: container %s", docker.ErrNotFound, nameOrID)
	}
	lines := append([]string(nil), c.logs...)
	if opts.Tail > 0 && len(lines) > opts.Tail {
		lines = lines[len(lines)-opts.Tail:]
	}
	sent := len(c.logs)
	unlock()

	if !opts.Follow {
		var buf bytes.Buffer
		for _, l := range lines {
			buf.WriteString(l + "\n")
		}
		return io.NopCloser(&buf), nil
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for _, l := range lines {
			if _, err := io.WriteString(pw, l+"\n"); err != nil {
				return
			}
		}
		for {
			f.mu.Lock()
			cur := f.ctrs[c.info.ID]
			var fresh []string
			running := false
			if cur != nil {
				fresh = append(fresh, cur.logs[sent:]...)
				sent = len(cur.logs)
				running = cur.info.State == docker.StateRunning
			}
			changed := f.changed
			f.mu.Unlock()
			for _, l := range fresh {
				if _, err := io.WriteString(pw, l+"\n"); err != nil {
					return
				}
			}
			if !running {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
		}
	}()
	return pr, nil
}
