// Command goproxy is a spike for rig's egress proxy: an allowlist-only HTTPS CONNECT
// tunnel. It never terminates TLS, so it cannot read traffic, and it refuses plain
// HTTP, non-listed hosts or ports, and any name that resolves to a non-public address.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type policy struct {
	hosts []string // exact names or "*.suffix"
	ports map[string]bool
	priv  bool // allow private/loopback targets (off by default)
}

func (p policy) hostAllowed(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	for _, a := range p.hosts {
		if a == h || (strings.HasPrefix(a, "*.") && strings.HasSuffix(h, a[1:])) {
			return true
		}
	}
	return false
}

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast())
}

func logf(action, host, reason string) {
	json.NewEncoder(os.Stdout).Encode(map[string]string{
		"ts": time.Now().UTC().Format(time.RFC3339), "action": action, "host": host, "reason": reason,
	})
}

func (p policy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		logf("deny", r.Host, "plain http not allowed")
		http.Error(w, "rig egress: only HTTPS CONNECT is allowed", http.StatusForbidden)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	if !p.hostAllowed(host) || !p.ports[port] {
		logf("deny", r.Host, "not in allowlist")
		http.Error(w, "rig egress: host not allowed", http.StatusForbidden)
		return
	}
	ips, err := net.DefaultResolver.LookupIPAddr(r.Context(), host)
	if err != nil || len(ips) == 0 {
		logf("deny", r.Host, "dns failure")
		http.Error(w, "dns failure", http.StatusBadGateway)
		return
	}
	var target net.IP
	for _, a := range ips {
		if p.priv || publicIP(a.IP) {
			target = a.IP
			break
		}
	}
	if target == nil { // dial the vetted IP itself, so DNS rebinding cannot swap it
		logf("deny", r.Host, "resolves to non-public address")
		http.Error(w, "rig egress: non-public address", http.StatusForbidden)
		return
	}
	up, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(context.Background(), "tcp", net.JoinHostPort(target.String(), port))
	if err != nil {
		logf("deny", r.Host, "dial failed")
		http.Error(w, "dial failed", http.StatusBadGateway)
		return
	}
	down, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	logf("allow", r.Host, "")
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

func main() {
	p := policy{ports: map[string]bool{}, priv: os.Getenv("ALLOW_PRIVATE") == "1"}
	for _, h := range strings.Split(os.Getenv("ALLOW_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			p.hosts = append(p.hosts, h)
		}
	}
	ports := os.Getenv("ALLOW_PORTS")
	if ports == "" {
		ports = "443"
	}
	for _, port := range strings.Split(ports, ",") {
		p.ports[strings.TrimSpace(port)] = true
	}
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":3128"
	}
	srv := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	logf("start", addr, strings.Join(p.hosts, ","))
	if err := srv.ListenAndServe(); err != nil {
		logf("fatal", "", err.Error())
		os.Exit(1)
	}
}
