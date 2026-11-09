package egress_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/egress"
)

type logbook struct {
	mu      sync.Mutex
	entries []egress.Entry
}

func (l *logbook) add(e egress.Entry) { l.mu.Lock(); l.entries = append(l.entries, e); l.mu.Unlock() }
func (l *logbook) all() []egress.Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]egress.Entry(nil), l.entries...)
}

// echoServer accepts connections and echoes what it reads, then closes on EOF.
func echoServer(t *testing.T) (port int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func startProxy(t *testing.T, o egress.Options) (addr string, log *logbook) {
	t.Helper()
	log = &logbook{}
	o.Log = log.add
	srv := httptest.NewServer(egress.Handler(o))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), log
}

// connect sends a CONNECT and returns the status line plus the open connection.
func connect(t *testing.T, proxy, target string) (status string, conn net.Conn, br *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br = bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the proxy's answer: %v", err)
	}
	return strings.TrimSpace(line), conn, br
}

func TestAnAllowedHostIsTunneledBothWays(t *testing.T) {
	port := echoServer(t)
	addr, log := startProxy(t, egress.Options{Policy: egress.Policy{Hosts: []string{"localhost"}, Ports: []int{port}, AllowPrivate: true}})
	status, conn, br := connect(t, addr, fmt.Sprintf("localhost:%d", port))
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("status = %q", status)
	}
	for br.Buffered() > 0 { // the blank line ending the response head
		br.ReadString('\n')
		break
	}
	conn.Write([]byte("ping through the tunnel"))
	got := make([]byte, len("ping through the tunnel"))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping through the tunnel" {
		t.Errorf("echo = %q, %v", got, err)
	}
	conn.(*net.TCPConn).CloseWrite() // a half-close must reach the target and come back
	if rest, _ := io.ReadAll(br); len(rest) != 0 {
		t.Errorf("unexpected trailing bytes %q", rest)
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(log.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e := log.all(); len(e) != 1 || e[0].Action != "allow" || e[0].Host != fmt.Sprintf("localhost:%d", port) {
		t.Errorf("log = %+v", e)
	}
}

func TestRefusals(t *testing.T) {
	port := echoServer(t)
	dead := closedPort(t)
	allowed := egress.Policy{Hosts: []string{"api.example.com", "*.example.org"}, Ports: []int{443}}
	tests := []struct {
		name, target, wantStatus, wantReason string
		policy                               egress.Policy
		resolver                             egress.Resolver
	}{
		{"a host that is not allowed", "evil.com:443", "403", "not in allowlist", allowed, nil},
		{"an allowed host on another port", "api.example.com:8443", "403", "not in allowlist", allowed, nil},
		{"the wildcard's bare domain", "example.org:443", "403", "not in allowlist", allowed, nil},
		{"a suffix lookalike", "evilexample.org:443", "403", "not in allowlist", allowed, nil},
		{"a wildcard as a prefix of another name", "a.example.org.evil.com:443", "403", "not in allowlist", allowed, nil},
		{"an allowlisted name that resolves to loopback", "api.example.com:443", "403", "non-public", allowed, resolveTo("127.0.0.1")},
		{"an allowlisted name that resolves to a private address", "api.example.com:443", "403", "non-public", allowed, resolveTo("10.0.0.5")},
		{"an allowlisted name that resolves to a link-local address", "api.example.com:443", "403", "non-public", allowed, resolveTo("169.254.169.254")},
		{"an allowlisted name that resolves to the unspecified address", "api.example.com:443", "403", "non-public", allowed, resolveTo("0.0.0.0")},
		{"an allowlisted name that resolves only to private addresses", "api.example.com:443", "403", "non-public", allowed, resolveTo("192.168.1.1", "fd00::1")},
		{"a name that does not resolve", "api.example.com:443", "502", "dns failure", allowed, func(context.Context, string) ([]net.IP, error) { return nil, fmt.Errorf("no such host") }},
		{"a name with no addresses", "api.example.com:443", "502", "dns failure", allowed, resolveTo()},
		{"a target that refuses connections", fmt.Sprintf("localhost:%d", dead), "502", "dial failed",
			egress.Policy{Hosts: []string{"localhost"}, Ports: []int{dead}, AllowPrivate: true}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, log := startProxy(t, egress.Options{Policy: tt.policy, Resolver: tt.resolver})
			status, _, _ := connect(t, addr, tt.target)
			if !strings.Contains(status, " "+tt.wantStatus+" ") {
				t.Errorf("status = %q, want %s", status, tt.wantStatus)
			}
			e := log.all()
			if len(e) != 1 || e[0].Action != "deny" || !strings.Contains(e[0].Reason, tt.wantReason) || e[0].Host != tt.target {
				t.Errorf("log = %+v, want one deny %q for %s", e, tt.wantReason, tt.target)
			}
		})
	}
	_ = port
}

func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func resolveTo(ips ...string) egress.Resolver {
	return func(context.Context, string) ([]net.IP, error) {
		var out []net.IP
		for _, s := range ips {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func TestPlainHTTPAndMalformedRequestsAreRefused(t *testing.T) {
	addr, log := startProxy(t, egress.Options{Policy: egress.Policy{Hosts: []string{"api.example.com"}, Ports: []int{443}}})
	resp, err := http.Get("http://" + addr + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a plain request got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodConnect, "http://"+addr, nil)
	req.Host = "no-port-here"
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("a CONNECT without a port got %d", resp.StatusCode)
		}
	}
	if e := log.all(); len(e) == 0 || e[0].Reason != "plain http not allowed" {
		t.Errorf("log = %+v", e)
	}
}

func TestWildcardAndCaseHandling(t *testing.T) {
	port := echoServer(t)
	addr, _ := startProxy(t, egress.Options{
		Policy:   egress.Policy{Hosts: []string{"*.Anthropic.com", "Exact.Example.net"}, Ports: []int{port}, AllowPrivate: true},
		Resolver: resolveTo("127.0.0.1"),
	})
	tests := map[string]string{
		fmt.Sprintf("api.anthropic.com:%d", port):          "200",
		fmt.Sprintf("API.ANTHROPIC.COM:%d", port):          "200",
		fmt.Sprintf("a.b.anthropic.com:%d", port):          "200",
		fmt.Sprintf("api.anthropic.com.:%d", port):         "200", // trailing dot
		fmt.Sprintf("exact.example.net:%d", port):          "200",
		fmt.Sprintf("anthropic.com:%d", port):              "403",
		fmt.Sprintf("notanthropic.com:%d", port):           "403",
		fmt.Sprintf("api.anthropic.com.evil.com:%d", port): "403",
		fmt.Sprintf("sub.exact.example.net:%d", port):      "403",
	}
	for target, want := range tests {
		status, conn, _ := connect(t, addr, target)
		conn.Close()
		if !strings.Contains(status, " "+want+" ") {
			t.Errorf("%s: status %q, want %s", target, status, want)
		}
	}
}

func TestTheVettedAddressIsTheOneDialed(t *testing.T) {
	port := echoServer(t)
	calls := 0
	addr, _ := startProxy(t, egress.Options{
		Policy: egress.Policy{Hosts: []string{"service.allowed.test"}, Ports: []int{port}, AllowPrivate: true},
		Resolver: func(context.Context, string) ([]net.IP, error) {
			calls++
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
	})
	// the name does not exist in DNS; reaching the echo server proves the resolver's
	// address was dialed, and that it was asked exactly once
	status, conn, _ := connect(t, addr, fmt.Sprintf("service.allowed.test:%d", port))
	conn.Close()
	if !strings.Contains(status, " 200 ") || calls != 1 {
		t.Errorf("status %q after %d lookups", status, calls)
	}
}

func TestOnlyVettedAddressesAreDialedAndTheFirstThatConnectsWins(t *testing.T) {
	var dialed []string
	failFirst := true
	addr, log := startProxy(t, egress.Options{
		Policy:   egress.Policy{Hosts: []string{"api.example.com"}, Ports: []int{443}},
		Resolver: resolveTo("10.1.1.1", "2001:db8::1", "203.0.113.7", "127.0.0.1"),
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			if failFirst {
				failFirst = false
				return nil, fmt.Errorf("unreachable")
			}
			c1, c2 := net.Pipe()
			go io.Copy(io.Discard, c2)
			return c1, nil
		},
	})
	status, conn, _ := connect(t, addr, "api.example.com:443")
	conn.Close()
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status = %q", status)
	}
	want := []string{"[2001:db8::1]:443", "203.0.113.7:443"}
	if strings.Join(dialed, ",") != strings.Join(want, ",") {
		t.Errorf("dialed %v, want %v: the private and loopback answers are never tried, and a failure moves on to the next", dialed, want)
	}
	_ = log
}

func TestWhenNoVettedAddressConnectsTheDialFails(t *testing.T) {
	addr, log := startProxy(t, egress.Options{
		Policy:   egress.Policy{Hosts: []string{"api.example.com"}, Ports: []int{443}},
		Resolver: resolveTo("203.0.113.7", "198.51.100.9"),
		Dial:     func(context.Context, string, string) (net.Conn, error) { return nil, fmt.Errorf("unreachable") },
	})
	status, _, _ := connect(t, addr, "api.example.com:443")
	if e := log.all(); !strings.Contains(status, " 502 ") || len(e) != 1 || e[0].Reason != "dial failed" {
		t.Errorf("status %q, log %+v", status, e)
	}
}

func TestTooManyConnectionsAreRefused(t *testing.T) {
	port := echoServer(t)
	addr, log := startProxy(t, egress.Options{Policy: egress.Policy{Hosts: []string{"localhost"}, Ports: []int{port}, AllowPrivate: true}, MaxConns: 1})
	status, first, _ := connect(t, addr, fmt.Sprintf("localhost:%d", port))
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("first tunnel: %q", status)
	}
	defer first.Close()
	status, _, _ = connect(t, addr, fmt.Sprintf("localhost:%d", port))
	if !strings.Contains(status, " 503 ") {
		t.Errorf("second tunnel: %q, want 503 while the first is open", status)
	}
	found := false
	for _, e := range log.all() {
		found = found || e.Reason == "too many connections"
	}
	if !found {
		t.Errorf("log = %+v", log.all())
	}
}

func TestLogLinesAreJSON(t *testing.T) {
	var buf bytes.Buffer
	logf := egress.JSONLog(&buf)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	logf(egress.Entry{TS: now, Action: "deny", Host: "evil.com:443", Reason: "not in allowlist"})
	logf(egress.Entry{TS: now, Action: "allow", Host: "api.anthropic.com:443"})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log = %q", buf.String())
	}
	var e egress.Entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.Action != "deny" || e.Host != "evil.com:443" || e.Reason != "not in allowlist" || !e.TS.Equal(now) {
		t.Errorf("entry = %+v, %v", e, err)
	}
	if strings.Contains(lines[1], "reason") {
		t.Errorf("an empty reason should be left out: %s", lines[1])
	}
}

func TestPolicyValidation(t *testing.T) {
	tests := []struct {
		name string
		p    egress.Policy
		ok   bool
	}{
		{"typical", egress.Policy{Hosts: []string{"api.anthropic.com", "*.npmjs.org"}, Ports: []int{443}}, true},
		{"no hosts at all", egress.Policy{Ports: []int{443}}, true},
		{"a bare wildcard", egress.Policy{Hosts: []string{"*"}, Ports: []int{443}}, false},
		{"a wildcard with no domain", egress.Policy{Hosts: []string{"*."}, Ports: []int{443}}, false},
		{"a scheme", egress.Policy{Hosts: []string{"https://x.com"}, Ports: []int{443}}, false},
		{"a space", egress.Policy{Hosts: []string{"a b.com"}, Ports: []int{443}}, false},
		{"a leading dot", egress.Policy{Hosts: []string{".x.com"}, Ports: []int{443}}, false},
		{"a double dot", egress.Policy{Hosts: []string{"a..com"}, Ports: []int{443}}, false},
		{"a port that is too high", egress.Policy{Ports: []int{70000}}, false},
		{"port zero", egress.Policy{Ports: []int{0}}, false},
	}
	for _, tt := range tests {
		if err := tt.p.Validate(); (err == nil) != tt.ok {
			t.Errorf("%s: Validate = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}
