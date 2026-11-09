package egress

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Docker publishes a container's ports to the host only when the container is on a
// network with a route out, and an internal network has none by design. So the proxy
// container, which sits on both the internal network and a normal bridge, also carries
// the ticket's published ports inbound: the host reaches localhost:<port>, Docker
// delivers it to the proxy, and the proxy hands it to the service on the internal
// network. Traffic only ever flows to the listed targets, and only inward.

// Forward sends connections arriving on a port to one fixed target.
type Forward struct {
	ListenPort int
	Target     string // host:port on the internal network
}

// ParseForwards parses "41000=web:8080,41001=api:3000".
func ParseForwards(s string) ([]Forward, error) {
	var out []Forward
	seen := map[int]bool{}
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		port, target, ok := strings.Cut(item, "=")
		listen, err := strconv.Atoi(strings.TrimSpace(port))
		if !ok || err != nil || listen < 1 || listen > 65535 {
			return nil, fmt.Errorf("forward %q must look like 41000=service:8080", item)
		}
		host, targetPort, err := net.SplitHostPort(strings.TrimSpace(target))
		if err != nil || host == "" {
			return nil, fmt.Errorf("forward %q: the target must be host:port", item)
		}
		if n, err := strconv.Atoi(targetPort); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("forward %q: %q is not a port", item, targetPort)
		}
		if seen[listen] {
			return nil, fmt.Errorf("forward port %d is listed twice", listen)
		}
		seen[listen] = true
		out = append(out, Forward{ListenPort: listen, Target: strings.TrimSpace(target)})
	}
	return out, nil
}

// FormatForwards is the inverse of ParseForwards.
func FormatForwards(forwards []Forward) string {
	parts := make([]string, len(forwards))
	for i, f := range forwards {
		parts[i] = strconv.Itoa(f.ListenPort) + "=" + f.Target
	}
	return strings.Join(parts, ",")
}

// ServeForward listens on the forward's port until ctx ends and relays every
// connection to its target. The listener is returned so callers can learn the bound
// address when the port was 0 (tests).
func ServeForward(ctx context.Context, f Forward, listener net.Listener, dial func(ctx context.Context, network, address string) (net.Conn, error), log func(Entry)) {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	if log == nil {
		log = func(Entry) {}
	}
	go func() { <-ctx.Done(); listener.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		down, err := listener.Accept()
		if err != nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer down.Close()
			up, err := dial(ctx, "tcp", f.Target)
			if err != nil {
				log(Entry{TS: time.Now().UTC(), Action: "forward-failed", Host: f.Target, Reason: err.Error()})
				return
			}
			defer up.Close()
			var pair sync.WaitGroup
			pair.Add(2)
			splice := func(dst, src net.Conn) {
				defer pair.Done()
				io.Copy(dst, src)
				if c, ok := dst.(*net.TCPConn); ok {
					c.CloseWrite()
				}
			}
			go splice(up, down)
			go splice(down, up)
			pair.Wait()
		}()
	}
}
