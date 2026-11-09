// Package docker is how rig runs a ticket's containers: one network per ticket,
// service and dependency containers on it, and (for sandboxed agents) agent
// containers. Everything rig creates carries labels (LabelTicket, LabelService,
// LabelRole) so cleanup and reconciliation can find it later. The real client and the
// in-memory fake (testkit.FakeDocker) both pass the suite in package dockertest.
package docker

import (
	"context"
	"errors"
	"io"
	"time"
)

// Labels put on every Docker object rig creates (docs/formats.md §7).
const (
	LabelTicket  = "rig.ticket"
	LabelService = "rig.service"
	LabelRole    = "rig.role"
)

// Roles for the rig.role label.
const (
	RoleService    = "service"
	RoleAgent      = "agent"
	RoleDependency = "dependency"
	RoleProxy      = "proxy"
	RoleNetwork    = "network"
)

// Labels builds the labels every object of a ticket carries; service may be empty
// (networks and proxies belong to the ticket as a whole).
func Labels(ticket, service, role string) map[string]string {
	l := map[string]string{LabelTicket: ticket, LabelRole: role}
	if service != "" {
		l[LabelService] = service
	}
	return l
}

var (
	// ErrNotFound: no such network, container or image.
	ErrNotFound = errors.New("docker object not found")
	// ErrExists: the name is taken.
	ErrExists = errors.New("docker name already in use")
	// ErrInUse: the operation needs something that is held, such as a host port that
	// another container has bound, or a network that still has containers on it.
	ErrInUse = errors.New("docker resource in use")
	// ErrNotRunning: the container exists but is not running (exec needs it to be).
	ErrNotRunning = errors.New("container is not running")
	// ErrUnavailable: the Docker daemon cannot be reached.
	ErrUnavailable = errors.New("docker unavailable")
	// ErrInvalid: the request itself is wrong (a missing mount source, no image).
	ErrInvalid = errors.New("invalid docker request")
)

type Docker interface {
	// Ping succeeds when the daemon can be reached; otherwise it explains why
	// (ErrUnavailable).
	Ping(ctx context.Context) error

	// Networks are referred to by name or id.
	CreateNetwork(ctx context.Context, spec NetworkSpec) (NetworkInfo, error)
	InspectNetwork(ctx context.Context, nameOrID string) (NetworkInfo, error)
	// ListNetworks returns the networks carrying every given label (key=value).
	ListNetworks(ctx context.Context, labels map[string]string) ([]NetworkInfo, error)
	// RemoveNetwork fails with ErrInUse while containers are attached.
	RemoveNetwork(ctx context.Context, nameOrID string) error

	// BuildImage builds Spec.Tag from a Dockerfile. A failed build returns an error
	// that ends with the tail of the build output.
	BuildImage(ctx context.Context, spec BuildSpec) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ImageID returns the content id of a local image (its digest), so a cache can tell
	// when a mutable tag such as golang:1.24 has moved. ErrNotFound if it is not local.
	ImageID(ctx context.Context, ref string) (string, error)

	// CreateContainer makes a stopped container; StartContainer runs it. Containers
	// are referred to by name or id.
	CreateContainer(ctx context.Context, spec ContainerSpec) (ContainerInfo, error)
	StartContainer(ctx context.Context, nameOrID string) error
	StopContainer(ctx context.Context, nameOrID string, grace time.Duration) error
	// RemoveContainer fails with ErrInUse for a running container unless force.
	RemoveContainer(ctx context.Context, nameOrID string, force bool) error
	InspectContainer(ctx context.Context, nameOrID string) (ContainerInfo, error)
	// ListContainers returns every container (running or not) carrying every given label.
	ListContainers(ctx context.Context, labels map[string]string) ([]ContainerInfo, error)

	// Exec runs a command to completion inside a running container.
	Exec(ctx context.Context, nameOrID string, spec ExecSpec) (ExecResult, error)
	// Logs returns the container's output (stdout and stderr merged, as text). It
	// works after the container has exited. With Follow it keeps streaming until the
	// context ends or the container stops; the caller closes the reader.
	Logs(ctx context.Context, nameOrID string, opts LogOptions) (io.ReadCloser, error)
}

type NetworkSpec struct {
	Name     string
	Internal bool // no route out of the network (docs/spikes/05-egress.md)
	Labels   map[string]string
}

type NetworkInfo struct {
	ID       string
	Name     string
	Internal bool
	Labels   map[string]string
	// Containers lists the ids of the containers attached to it.
	Containers []string
}

type BuildSpec struct {
	Tag        string
	ContextDir string // an absolute directory
	Dockerfile string // relative to ContextDir; default "Dockerfile"
	Args       map[string]string
	Labels     map[string]string
}

type ContainerSpec struct {
	Name    string
	Image   string
	Command []string // empty means the image's own
	Env     map[string]string
	Labels  map[string]string
	// WorkDir and User are the image's when empty. User is "uid:gid" or a name.
	WorkDir string
	User    string
	Mounts  []Mount
	Ports   []PortBinding
	// Network is the network to join; Aliases are extra names peers on it can use
	// (the container's own name always works).
	Network string
	Aliases []string
	// Healthcheck, when set, makes Health move from starting to healthy or unhealthy.
	Healthcheck *Healthcheck
	// TTY and Stdin make the container usable by `docker exec -it` style sessions.
	TTY   bool
	Stdin bool
}

// Mount is a bind mount from the host. The source must exist.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// PortBinding publishes a container port on the host's loopback interface. Host 0
// lets Docker choose; the chosen port shows in ContainerInfo.Ports once the container
// runs.
type PortBinding struct {
	Container int
	Host      int
}

type Healthcheck struct {
	Command  []string // run inside the container; exit 0 is healthy
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
}

type ContainerState string

const (
	StateCreated ContainerState = "created"
	StateRunning ContainerState = "running"
	StateExited  ContainerState = "exited"
)

type Health string

const (
	HealthNone      Health = "none" // no healthcheck configured
	HealthStarting  Health = "starting"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

type ContainerInfo struct {
	ID       string
	Name     string
	Image    string
	State    ContainerState
	ExitCode int // meaningful when State is exited
	Health   Health
	Labels   map[string]string
	Env      map[string]string
	Mounts   []Mount
	// Ports are the bindings in effect; Host is the real port even if 0 was asked for.
	Ports []PortBinding
	// Networks maps each network the container is on to the names peers can reach it by.
	Networks map[string]ContainerNetwork
}

type ContainerNetwork struct {
	Aliases []string // includes the container's own name
}

type ExecSpec struct {
	Command []string
	Env     map[string]string
	User    string
	WorkDir string
}

type ExecResult struct {
	ExitCode int
	Output   string // stdout and stderr merged
}

type LogOptions struct {
	Tail   int // the last N lines; 0 means all
	Follow bool
}
