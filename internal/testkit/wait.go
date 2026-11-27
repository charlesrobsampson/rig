package testkit

import (
	"net"
	"testing"
	"time"
)

// WaitLimit is how long a test waits for something to happen before it calls the
// test hung. It is only a guard against hanging forever, so it is far longer than
// anything a loaded machine needs; a test never asserts that something was quick.
const WaitLimit = 60 * time.Second

// Eventually polls cond until it is true and fails the test after WaitLimit. A cond
// may do work each round, such as advancing a fake clock.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(WaitLimit) // real time: only the hang guard
	for !cond() {
		if time.Now().After(deadline) { // real time: only the hang guard
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// GuardConn makes a blocked read or write on conn fail after WaitLimit instead of
// hanging the test.
func GuardConn(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(WaitLimit)) // real time: only the hang guard
}
