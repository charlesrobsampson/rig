// Command rig-egress is the allowlist proxy that runs in a ticket's proxy container. It
// is built static for linux and embedded in the rig binary (task build:egress); rig
// configures it entirely through the environment: ALLOW_HOSTS (comma list of names or
// *.suffix), ALLOW_PORTS (default 443), LISTEN (default :3128), and FORWARD
// (port=service:port pairs that carry published ports inbound to services).
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charlesrobsampson/rig/internal/egress"
)

func main() { os.Exit(run()) }

func run() int {
	logf := egress.JSONLog(os.Stdout)
	policy, err := policyFromEnv(os.Getenv)
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":3128"
	}
	if err != nil {
		logf(egress.Entry{TS: time.Now().UTC(), Action: "fatal", Reason: err.Error()})
		return 2
	}
	srv := &http.Server{Addr: addr, Handler: egress.Handler(egress.Options{Policy: policy, Log: logf}), ReadHeaderTimeout: 10 * time.Second}
	logf(egress.Entry{TS: time.Now().UTC(), Action: "start", Host: addr, Reason: strings.Join(policy.Hosts, ",")})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	forwards, err := egress.ParseForwards(os.Getenv("FORWARD"))
	if err != nil {
		logf(egress.Entry{TS: time.Now().UTC(), Action: "fatal", Reason: err.Error()})
		return 2
	}
	for _, f := range forwards {
		l, err := net.Listen("tcp", ":"+strconv.Itoa(f.ListenPort))
		if err != nil {
			logf(egress.Entry{TS: time.Now().UTC(), Action: "fatal", Reason: err.Error()})
			return 1
		}
		logf(egress.Entry{TS: time.Now().UTC(), Action: "forward", Host: f.Target, Reason: "port " + strconv.Itoa(f.ListenPort)})
		go egress.ServeForward(ctx, f, l, nil, logf)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logf(egress.Entry{TS: time.Now().UTC(), Action: "fatal", Reason: err.Error()})
		return 1
	}
	return 0
}

func policyFromEnv(getenv func(string) string) (egress.Policy, error) {
	p := egress.Policy{AllowPrivate: getenv("ALLOW_PRIVATE") == "1"}
	for _, h := range strings.Split(getenv("ALLOW_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			p.Hosts = append(p.Hosts, h)
		}
	}
	ports := getenv("ALLOW_PORTS")
	if ports == "" {
		ports = "443"
	}
	for _, s := range strings.Split(ports, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return p, errors.New("ALLOW_PORTS: " + s + " is not a port number")
		}
		p.Ports = append(p.Ports, n)
	}
	return p, p.Validate()
}
