package main

import (
	"net"
	"testing"
	"time"
)

// TestConnTracker_ShutdownEndsIdleConnectionsPromptly: a connection
// blocked waiting for its next request must end as soon as shutdown
// begins, not hold the daemon open until the drain timeout.
func TestConnTracker_ShutdownEndsIdleConnectionsPromptly(t *testing.T) {
	conns := newConnTracker()
	serverEnd, clientEnd := net.Pipe()
	defer clientEnd.Close()

	if !conns.add(serverEnd) {
		t.Fatal("add before shutdown returned false")
	}
	go func() {
		defer conns.remove(serverEnd)
		serverEnd.Read(make([]byte, 1)) // an idle session waiting for a request
	}()

	conns.beginShutdown()
	start := time.Now()
	conns.drain(5 * time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("drain took %v, want an idle connection to end right away", elapsed)
	}
	if conns.add(serverEnd) {
		t.Error("add after shutdown returned true, want new connections refused")
	}
}

func TestConnTracker_DrainForceClosesAfterTimeout(t *testing.T) {
	conns := newConnTracker()
	serverEnd, clientEnd := net.Pipe()
	defer clientEnd.Close()

	conns.add(serverEnd)
	go func() {
		defer conns.remove(serverEnd)
		// Simulates a request still being answered: it ignores the read
		// deadline and only stops once its connection is closed.
		serverEnd.Write(make([]byte, 1))
	}()

	conns.beginShutdown()
	start := time.Now()
	conns.drain(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("drain took %v, want it to force-close after its timeout", elapsed)
	}
}
