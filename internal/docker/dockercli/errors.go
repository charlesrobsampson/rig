package dockercli

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/charlesrobsampson/rig/internal/docker"
)

// mapError turns what `docker` printed into a docker error. The matched phrases are
// Docker's own messages; anything unrecognized stays a plain error carrying the text.
func mapError(text string) error {
	msg := strings.TrimSpace(text)
	lower := strings.ToLower(msg)
	wrap := func(sentinel error) error { return fmt.Errorf("%w: %s", sentinel, firstLine(msg)) }
	switch {
	case strings.Contains(lower, "cannot connect to the docker daemon"),
		strings.Contains(lower, "is the docker daemon running"),
		strings.Contains(lower, "error during connect"),
		strings.Contains(lower, "docker daemon is not running"),
		strings.Contains(lower, "failed to connect to the docker api"):
		return fmt.Errorf("%w: the Docker daemon cannot be reached; start Docker (%s)", docker.ErrUnavailable, firstLine(msg))
	case strings.Contains(lower, "no such network"), strings.Contains(lower, "no such container"),
		strings.Contains(lower, "no such object"), strings.Contains(lower, "no such image"),
		strings.Contains(lower, "network") && strings.Contains(lower, "not found"):
		return wrap(docker.ErrNotFound)
	case strings.Contains(lower, "pull access denied"), strings.Contains(lower, "manifest unknown"),
		strings.Contains(lower, "repository does not exist"), strings.Contains(lower, "unable to find image") && strings.Contains(lower, "not found"):
		return wrap(docker.ErrNotFound)
	case strings.Contains(lower, "is already in use by container"), strings.Contains(lower, "already exists"):
		return wrap(docker.ErrExists)
	case strings.Contains(lower, "port is already allocated"), strings.Contains(lower, "address already in use"),
		strings.Contains(lower, "has active endpoints"), strings.Contains(lower, "cannot remove a running container"), strings.Contains(lower, "container is running"),
		strings.Contains(lower, "is in use"):
		return wrap(docker.ErrInUse)
	case strings.Contains(lower, "is not running"):
		return wrap(docker.ErrNotRunning)
	}
	return errors.New(firstLine(msg))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// syncWriter lets stdout and stderr share one buffer from two goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  interface{ Write([]byte) (int, error) }
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
