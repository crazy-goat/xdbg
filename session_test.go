package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func dbgpPacket(xml string) string { return fmt.Sprintf("%d\x00%s\x00", len(xml), xml) }

const xmlProlog = `<?xml version="1.0" encoding="iso-8859-1"?>`

// fakeEngine is the Xdebug side of an in-memory connection.
type fakeEngine struct {
	t    *testing.T
	conn net.Conn
}

func newPipe(t *testing.T) (*fakeEngine, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return &fakeEngine{t: t, conn: client}, server
}

func (e *fakeEngine) send(xml string) {
	e.t.Helper()
	_ = e.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := e.conn.Write([]byte(dbgpPacket(xml))); err != nil {
		e.t.Errorf("engine write: %v", err)
	}
}

// drain reads and discards the commands the debugger sends.
func (e *fakeEngine) drain() {
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := e.conn.Read(buf); err != nil {
				return
			}
		}
	}()
}

func TestRawLockedMalformedResponse(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	eng.drain()
	go eng.send(xmlProlog + `<response command="eval" status="break"`) // truncated XML

	r, raw, err := s.cmd("eval", "-- x")
	if err == nil {
		t.Fatalf("want an error for malformed XML, got response %+v", r)
	}
	if !strings.Contains(err.Error(), "parse eval response") {
		t.Fatalf("error should name the command, got %v", err)
	}
	if r != nil || !strings.Contains(raw, "<response") {
		t.Fatalf("want nil response and the raw packet, got %+v / %q", r, raw)
	}
	if s.state != "started" {
		t.Fatalf("a malformed response must not change the state, got %q", s.state)
	}
}

func TestRawLockedWellFormedResponse(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	eng.drain()
	go eng.send(xmlProlog + `<response command="run" status="break"/>`)

	r, _, err := s.cmd("run", "")
	if err != nil || r == nil || r.Status != "break" || s.state != "break" {
		t.Fatalf("got %+v, %v, state %q", r, err, s.state)
	}
}

func TestAdoptMalformedInit(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	ready := s.ready
	eng.drain()
	go eng.send(xmlProlog + `<init fileuri="file:///d/index.php"`) // truncated XML

	s.adopt(conn)

	if s.state != "no session" {
		t.Fatalf("state = %q, want %q", s.state, "no session")
	}
	if s.conn != nil {
		t.Fatal("the connection must be dropped after a malformed init packet")
	}
	select {
	case <-ready:
		t.Fatal("a failed handshake must not signal ready")
	default:
	}
}

func TestAdoptWellFormedInit(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.pending = []bp{{file: "/d/a.php", line: 3}}
	go func() {
		eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
		buf := make([]byte, 4096)
		// Answer every command (3x feature_set, breakpoint_set) with a generic response.
		for i := 0; i < 4; i++ {
			if _, err := eng.conn.Read(buf); err != nil {
				return
			}
			eng.send(xmlProlog + `<response command="x" id="7"/>`)
		}
	}()

	s.adopt(conn)

	if s.state != "started" || s.conn == nil {
		t.Fatalf("state = %q, conn nil = %v", s.state, s.conn == nil)
	}
	if s.pending[0].id != "7" {
		t.Fatalf("pending breakpoint id = %q, want 7", s.pending[0].id)
	}
}
