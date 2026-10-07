package main

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// listenerAddr returns the address of the session's open DBGp listener, or ""
// when no listener is open.
func listenerAddr(s *session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// TestRunCommandFailsFastWhenCommandExits checks that a container command that
// fails at once (bad service, typo, container down) returns promptly with the
// command output instead of waiting for the whole timeout and hiding it.
func TestRunCommandFailsFastWhenCommandExits(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = "127.0.0.1:0"
	s.containerExec = "sh -c 'echo no such service: php >&2; exit 1' --"

	start := time.Now()
	_, err := s.RunCommand("bin/console app:x", 3*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunCommand = nil error, want a command failure")
	}
	if elapsed >= time.Second {
		t.Fatalf("RunCommand took %s; want it to return as soon as the command exits", elapsed)
	}
	if !strings.Contains(err.Error(), "no such service: php") {
		t.Fatalf("RunCommand error = %q; want the command output", err)
	}
	if got := listenerAddr(s); got != "" {
		t.Fatalf("listener still open at %s; want it closed", got)
	}
}

// TestRunCommandSucceedsWithoutXdebug checks that a command that succeeds while
// Xdebug stays off returns promptly with the no-Xdebug error and the output,
// instead of waiting for the whole timeout.
func TestRunCommandSucceedsWithoutXdebug(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = "127.0.0.1:0"
	s.containerExec = "echo"

	start := time.Now()
	_, err := s.RunCommand("hello", 3*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunCommand = nil error, want a no-Xdebug error")
	}
	if elapsed >= time.Second {
		t.Fatalf("RunCommand took %s; want it to return as soon as the command exits", elapsed)
	}
	if !strings.Contains(err.Error(), "without an Xdebug connection") || !strings.Contains(err.Error(), "hello") {
		t.Fatalf("RunCommand error = %q; want the no-Xdebug message with the output", err)
	}
	if got := listenerAddr(s); got != "" {
		t.Fatalf("listener still open at %s; want it closed", got)
	}
}

// TestAwaitCommandExitDropsBoundaryConnection covers the #27 failure mode in the
// run_command path: a connection accepted right at the commandExitGrace boundary
// (here simulated by an accepted-but-not-adopted connection) must not survive a
// command error as an orphan paused session.
func TestAwaitCommandExitDropsBoundaryConnection(t *testing.T) {
	s := newSession("/l", "/d")
	ready := s.ready
	// An empty acceptResult means the accept goroutine has not reported yet; the
	// seeded acceptedConn is a connection accepted at the boundary.
	acceptResult := make(chan error, 1)
	server, client := net.Pipe()
	defer client.Close()
	s.acceptedConn = server

	res := commandResult{out: "boom", err: errors.New("exit status 1")}

	err := s.awaitCommandExit(ready, acceptResult, res)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("awaitCommandExit = %v; want the command error with the output", err)
	}

	s.mu.Lock()
	accepted := s.acceptedConn
	conn := s.conn
	s.mu.Unlock()
	if accepted != nil {
		t.Fatal("accepted connection left behind; want it dropped")
	}
	if conn != nil {
		t.Fatal("engine connection left behind; want no session")
	}
	if got := s.Status(); !strings.Contains(got, "state=no session") {
		t.Fatalf("Status = %q; want no session after the command error", got)
	}
}

// TestAwaitCommandExitReportsBoundarySession is the other half of the boundary
// race: when the connection accepted at the commandExitGrace boundary finishes
// adopting, the command error must NOT be returned — the live session must be
// reported (RunCommand's post-wait state handling then returns the paused
// session or the command output). Regression for the round-2 N1 hole, where
// settleAccepted returned nil for a successful adopt.
func TestAwaitCommandExitReportsBoundarySession(t *testing.T) {
	s := newSession("/l", "/d")
	ready := s.ready
	// Empty acceptResult: the outer commandExitGrace timer wins the first
	// select, so settleAccepted runs before adopt finishes.
	acceptResult := make(chan error, 1)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Adopt completes during settleAccepted (after the outer grace timer),
	// which is exactly the boundary window.
	go func() {
		time.Sleep(commandExitGrace + 100*time.Millisecond)
		s.mu.Lock()
		s.conn = server
		s.state = "started"
		s.adoptErr = nil
		s.signalReadyLocked()
		s.mu.Unlock()
	}()

	res := commandResult{out: "boom", err: errors.New("exit status 1")}

	err := s.awaitCommandExit(ready, acceptResult, res)
	if err != nil {
		t.Fatalf("awaitCommandExit = %v; want the adopted session reported, not the command error", err)
	}
	if got := s.Status(); !strings.Contains(got, "state=started") {
		t.Fatalf("Status = %q; want the adopted session", got)
	}
}

// TestAcquireListenerPortZeroSkipsProbe pins the port-0 fast path: binding to
// port 0 must not run the slow lsof probe.
func TestAcquireListenerPortZeroSkipsProbe(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = "127.0.0.1:0"

	start := time.Now()
	ln, err := s.acquireListener(10 * time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("acquireListener = %v", err)
	}
	ln.Close()

	if elapsed > 250*time.Millisecond {
		t.Fatalf("acquireListener took %s; want the lsof probe skipped for port 0", elapsed)
	}
}
