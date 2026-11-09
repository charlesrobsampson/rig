package lifecycle_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

// logsFixture is a started ticket "demo" with a service svc1 and a dependency db.
func logsFixture(t *testing.T, mutate func(*lifecycle.Options)) (*fixture, *testkit.FakeDocker) {
	t.Helper()
	f, dk, _ := serviceFixture(t, mutate)
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, apiService)
	addDeps(t, path, "dependencies:\n  - name: db\n    image: postgres:16\n")
	f.mustStart("demo", domain.SandboxContainer)
	return f, dk
}

func collect(t *testing.T, f *fixture, p api.LogsParams) ([]api.LogLine, error) {
	t.Helper()
	var out []api.LogLine
	err := f.lc.StreamLogs(ctx, p, func(item any) error {
		out = append(out, item.(api.LogLine))
		return nil
	})
	return out, err
}

func lines(ls []api.LogLine) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Service + "|" + l.Line
	}
	return out
}

func TestLogsCarryTheServicePrefixAndCoverServicesAndDependencies(t *testing.T) {
	f, dk := logsFixture(t, nil)
	dk.Print("rig-demo-svc1", "listening on 8080\nGET /healthz 200")
	dk.Print("rig-demo-dep-db", "database system is ready")

	got, err := collect(t, f, api.LogsParams{Ticket: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"svc1|listening on 8080", "svc1|GET /healthz 200", "db|database system is ready"}
	if strings.Join(lines(got), "\n") != strings.Join(want, "\n") {
		t.Errorf("logs = %v, want %v", lines(got), want)
	}
}

func TestTailLimitsEachContainer(t *testing.T) {
	f, dk := logsFixture(t, nil)
	for i := 1; i <= 5; i++ {
		dk.Print("rig-demo-svc1", "web line "+fmtInt(i))
		dk.Print("rig-demo-dep-db", "db line "+fmtInt(i))
	}
	got, _ := collect(t, f, api.LogsParams{Ticket: "demo", Tail: 2})
	want := "svc1|web line 4,svc1|web line 5,db|db line 4,db|db line 5"
	if strings.Join(lines(got), ",") != want {
		t.Errorf("tail 2 = %v", lines(got))
	}
}

func TestOneContainerCanBeNamed(t *testing.T) {
	f, dk := logsFixture(t, nil)
	dk.Print("rig-demo-svc1", "from the service")
	dk.Print("rig-demo-dep-db", "from the database")
	for name, want := range map[string]string{"svc1": "svc1|from the service", "db": "db|from the database"} {
		got, err := collect(t, f, api.LogsParams{Ticket: "demo", Service: name})
		if err != nil || strings.Join(lines(got), ",") != want {
			t.Errorf("%s: %v, %v", name, lines(got), err)
		}
	}
}

func TestLogsOutliveTheContainersProcess(t *testing.T) {
	f, dk := logsFixture(t, nil)
	dk.Print("rig-demo-svc1", "last words before the crash")
	dk.Exit("rig-demo-svc1", 1)
	got, err := collect(t, f, api.LogsParams{Ticket: "demo", Service: "svc1"})
	if err != nil || len(got) != 1 || got[0].Line != "last words before the crash" {
		t.Errorf("logs after exit = %v, %v", lines(got), err)
	}
}

func TestTheProxyIsAServiceForLogs(t *testing.T) {
	f, dk, _ := internalFixture(t, nil)
	f.create("demo", testkit.NewRepo(t))
	f.mustStart("demo", domain.SandboxContainer)
	dk.Print("rig-demo-proxy", `{"action":"deny","host":"example.com:443"}`)
	got, err := collect(t, f, api.LogsParams{Ticket: "demo", Service: "proxy"})
	if err != nil || len(got) != 1 || got[0].Service != "proxy" || !strings.Contains(got[0].Line, "example.com") {
		t.Errorf("proxy logs = %v, %v", lines(got), err)
	}
}

func TestLogErrors(t *testing.T) {
	f, _ := logsFixture(t, nil)
	tests := []struct {
		name string
		p    api.LogsParams
		code api.ErrorCode
		msg  string
	}{
		{"an unknown ticket", api.LogsParams{Ticket: "ghost"}, api.CodeNotFound, "ghost"},
		{"an unknown service", api.LogsParams{Ticket: "demo", Service: "nope"}, api.CodeNotFound, "db, proxy, svc1"},
		{"an invalid ticket id", api.LogsParams{Ticket: "../x"}, api.CodeInvalid, ""},
		{"a negative tail", api.LogsParams{Ticket: "demo", Tail: -1}, api.CodeInvalid, "tail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := collect(t, f, tt.p)
			if apiCode(t, err) != tt.code || !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("error = %v, want %s mentioning %q", err, tt.code, tt.msg)
			}
		})
	}

	t.Run("a named service that was never started", func(t *testing.T) {
		g, _, _ := serviceFixture(t, nil)
		repo := testkit.NewRepo(t)
		withService(t, g.create("fresh", repo), repo, true, apiService)
		if _, err := collect(t, g, api.LogsParams{Ticket: "fresh", Service: "svc1"}); apiCode(t, err) != api.CodeNotRunning || !strings.Contains(err.Error(), "start the ticket") {
			t.Errorf("error = %v", err)
		}
		if got, err := collect(t, g, api.LogsParams{Ticket: "fresh"}); err != nil || len(got) != 0 {
			t.Errorf("all logs of a ticket with nothing running = %v, %v; want empty, not an error", got, err)
		}
	})
	t.Run("a daemon without Docker", func(t *testing.T) {
		g, _ := agentFixture(t, nil)
		g.create("demo", testkit.NewRepo(t))
		if _, err := collect(t, g, api.LogsParams{Ticket: "demo"}); apiCode(t, err) != api.CodeUnavailable {
			t.Errorf("error = %v", err)
		}
	})
}

func TestFollowStreamsNewLinesFromEveryContainerUntilCancelled(t *testing.T) {
	f, dk := logsFixture(t, nil)
	dk.Print("rig-demo-svc1", "before")

	fctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- f.lc.StreamLogs(fctx, api.LogsParams{Ticket: "demo", Follow: true}, func(item any) error {
			l := item.(api.LogLine)
			mu.Lock()
			got = append(got, l.Service+"|"+l.Line)
			mu.Unlock()
			return nil
		})
	}()
	seen := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, g := range got {
			if g == want {
				return true
			}
		}
		return false
	}
	wait := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if seen(want) {
				return
			}
		}
		t.Fatalf("never saw %q; got %v", want, got)
	}
	wait("svc1|before")
	dk.Print("rig-demo-dep-db", "db is up")
	wait("db|db is up")
	dk.Print("rig-demo-svc1", "request handled")
	wait("svc1|request handled")

	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end when the client left")
	}
}

func TestFollowEndsWhenEveryContainerHasStopped(t *testing.T) {
	f, dk := logsFixture(t, nil)
	done := make(chan error, 1)
	var mu sync.Mutex
	n := 0
	go func() {
		done <- f.lc.StreamLogs(ctx, api.LogsParams{Ticket: "demo", Follow: true}, func(any) error { mu.Lock(); n++; mu.Unlock(); return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	dk.Print("rig-demo-svc1", "bye")
	dk.Exit("rig-demo-svc1", 0)
	dk.Exit("rig-demo-dep-db", 0)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("following never ended after the containers stopped")
	}
	if n != 1 {
		t.Errorf("%d lines delivered", n)
	}
	_ = os.Stdout
}
