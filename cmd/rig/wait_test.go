package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWaitReturnsWhenSomethingHappensOrTimesOut(t *testing.T) {
	_, d, repo, _ := newEnvWithTerminal(t)
	newTicket(t, d, "demo", repo)
	rig(t, d, "start", "demo", "--sandbox", "host")

	if stdout, _, code := rig(t, d, "wait", "demo", "--phase", "plan"); code != 0 || !strings.Contains(stdout, "phase is plan") {
		t.Errorf("already in plan: code %d, %q", code, stdout)
	}
	start := time.Now()
	if _, stderr, code := rig(t, d, "wait", "demo", "--phase-request", "--timeout", "300ms"); code != 1 || !strings.Contains(stderr, "timed out") {
		t.Errorf("timeout: code %d, %q", code, stderr)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the timeout was not kept")
	}

	var wg sync.WaitGroup
	var stdout string
	var code int
	wg.Add(1)
	go func() {
		defer wg.Done()
		stdout, _, code = rig(t, d, "wait", "demo", "--record", "note", "--phase-request", "--timeout", "10s", "--json")
	}()
	time.Sleep(700 * time.Millisecond)
	t.Setenv("RIG_TICKET", "demo")
	t.Setenv("RIG_SERVICE", "svc1")
	rig(t, d, "agent-event", `{"kind":"note","text":"a question for you"}`)
	wg.Wait()
	var got struct {
		Matched string `json:"matched"`
		Record  *struct {
			Text string `json:"text"`
		} `json:"record"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); code != 0 || err != nil || got.Matched != "record" || got.Record == nil || got.Record.Text != "a question for you" {
		t.Errorf("code %d, %q, %v", code, stdout, err)
	}
}

func TestWaitNeedsSomethingToWaitFor(t *testing.T) {
	_, d, repo := newEnv(t)
	newTicket(t, d, "demo", repo)
	if _, stderr, code := rig(t, d, "wait", "demo"); code != 2 || !strings.Contains(stderr, "--record") {
		t.Errorf("code %d, %q", code, stderr)
	}
	if _, _, code := rig(t, d, "wait", "ghost", "--phase", "plan"); code != 5 {
		t.Errorf("unknown ticket: code %d", code)
	}
}
