// Package egress is the one way out of a ticket's internal Docker network: an
// allowlist-only HTTPS CONNECT proxy (docs/spikes/05-egress.md). It never terminates
// TLS, so it cannot read or change traffic. It refuses plain HTTP, hosts and ports that
// are not allowed, and any name that resolves to a non-public address, and it dials the
// address it vetted so a DNS answer cannot change between the check and the connection.
package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Policy says what may be reached.
type Policy struct {
	Hosts []string // exact names or "*.suffix"
	Ports []int
	// AllowPrivate lets a target resolve to a private or loopback address. Off in
	// production; tests need it to reach a server on this machine.
	AllowPrivate bool
}

// Entry is one line of the proxy's log (JSON), which rig turns into events.
type Entry struct {
	TS     time.Time `json:"ts"`
	Action string    `json:"action"` // start, allow, deny, fatal
	Host   string    `json:"host"`   // host:port for allow and deny
	Reason string    `json:"reason,omitempty"`
}

// Validate rejects a policy that cannot work or would allow too much.
func (p Policy) Validate() error {
	for _, h := range p.Hosts {
		if !ValidHost(h) {
			return fmt.Errorf("%q is not a hostname or *.suffix pattern", h)
		}
	}
	for _, port := range p.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d must be between 1 and 65535", port)
		}
	}
	return nil
}

// ValidHost reports whether h is an exact hostname or a *.suffix wildcard.
func ValidHost(h string) bool {
	h = strings.TrimPrefix(h, "*.")
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") || strings.Contains(h, "..") {
		return false
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func (p Policy) hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range p.Hosts {
		a = strings.ToLower(a)
		if a == host || (strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) && len(host) > len(a)-1) {
			return true
		}
	}
	return false
}

func (p Policy) portAllowed(port string) bool {
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	for _, allowed := range p.Ports {
		if allowed == n {
			return true
		}
	}
	return false
}

// PublicIP reports whether ip is an address on the public internet.
func PublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast())
}

// Resolver looks a name up. Tests replace it to stage DNS answers.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

func systemResolver(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

type Options struct {
	Policy   Policy
	Log      func(Entry)      // nil discards
	Resolver Resolver         // nil means the system resolver
	Now      func() time.Time // nil means time.Now
	// MaxConns bounds concurrent tunnels (default 256), so a runaway agent cannot
	// exhaust the proxy.
	MaxConns int
	// DialTimeout is how long a connection to a target may take (default 10s).
	DialTimeout time.Duration
	// Dial connects to a vetted address (host:port with the address as an IP); nil
	// dials over TCP. Tests replace it to see what would be dialed.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

type handler struct {
	o    Options
	slot chan struct{}
}

// Handler returns the proxy as an http.Handler.
func Handler(o Options) http.Handler {
	if o.Log == nil {
		o.Log = func(Entry) {}
	}
	if o.Resolver == nil {
		o.Resolver = systemResolver
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxConns == 0 {
		o.MaxConns = 256
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.Dial == nil {
		d := &net.Dialer{Timeout: o.DialTimeout}
		o.Dial = d.DialContext
	}
	return &handler{o: o, slot: make(chan struct{}, o.MaxConns)}
}

func (h *handler) log(action, host, reason string) {
	h.o.Log(Entry{TS: h.o.Now().UTC(), Action: action, Host: host, Reason: reason})
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		h.log("deny", r.Host, "plain http not allowed")
		http.Error(w, "rig egress: only HTTPS CONNECT is allowed", http.StatusForbidden)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	if !h.o.Policy.hostAllowed(host) || !h.o.Policy.portAllowed(port) {
		h.log("deny", r.Host, "not in allowlist")
		http.Error(w, "rig egress: host not allowed", http.StatusForbidden)
		return
	}
	select {
	case h.slot <- struct{}{}:
		defer func() { <-h.slot }()
	default:
		h.log("deny", r.Host, "too many connections")
		http.Error(w, "rig egress: too many connections", http.StatusServiceUnavailable)
		return
	}

	ips, err := h.o.Resolver(r.Context(), host)
	if err != nil || len(ips) == 0 {
		h.log("deny", r.Host, "dns failure")
		http.Error(w, "dns failure", http.StatusBadGateway)
		return
	}
	var vetted []net.IP
	for _, ip := range ips {
		if h.o.Policy.AllowPrivate || PublicIP(ip) {
			vetted = append(vetted, ip)
		}
	}
	if len(vetted) == 0 {
		h.log("deny", r.Host, "resolves to non-public address")
		http.Error(w, "rig egress: non-public address", http.StatusForbidden)
		return
	}
	// Dial the addresses that were vetted, not the name, so a second DNS answer cannot
	// swap one in; the first that connects wins (a name often has an IPv6 address first).
	var up net.Conn
	for _, ip := range vetted {
		if up, err = h.o.Dial(r.Context(), "tcp", net.JoinHostPort(ip.String(), port)); err == nil {
			break
		}
	}
	if err != nil {
		h.log("deny", r.Host, "dial failed")
		http.Error(w, "dial failed", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	down, _, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	h.log("allow", r.Host, "")
	down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var wg sync.WaitGroup
	wg.Add(2)
	splice := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if c, ok := dst.(*net.TCPConn); ok {
			c.CloseWrite()
		}
	}
	go splice(up, down)
	go splice(down, up)
	wg.Wait()
	up.Close()
	down.Close()
}

// JSONLog returns a Log function that writes one JSON object per line to w.
func JSONLog(w io.Writer) func(Entry) {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	return func(e Entry) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(e)
	}
}
