package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

func dbgpPacket(xml string) string { return fmt.Sprintf("%d\x00%s\x00", len(xml), xml) }

const xmlProlog = `<?xml version="1.0" encoding="iso-8859-1"?>`

// featureAsyncResponse answers adopt's `feature_get -n supports_async` with an
// engine that supports interrupting a running script.
const featureAsyncResponse = `<response command="feature_get"><property name="supports_async" type="int" encoding="none">1</property></response>`

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

func newActivePipe(t *testing.T) (*session, *fakeEngine) {
	t.Helper()
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn, s.r)
	return s, eng
}

// txOf returns the transaction id of a DBGp command line, for example "1" for
// "run -i 1", or "" when the line carries no -i.
func txOf(line string) string {
	line = strings.TrimSuffix(line, "\x00")
	parts := strings.SplitN(line, " ", 4) // name, "-i", tx, rest
	if len(parts) < 3 || parts[1] != "-i" {
		return ""
	}
	return parts[2]
}

// withTx adds transaction_id to a <response> that does not carry one, so the
// debugger's reply matching accepts it.
func withTx(response, tx string) string {
	if tx == "" || strings.Contains(response, "transaction_id=") {
		return response
	}
	return strings.Replace(response, "<response", `<response transaction_id="`+tx+`"`, 1)
}

func (e *fakeEngine) respond(responses ...string) {
	e.t.Helper()
	r := bufio.NewReader(e.conn)
	for _, response := range responses {
		line, err := r.ReadString(0)
		if err != nil {
			return
		}
		e.send(xmlProlog + withTx(response, txOf(line)))
	}
}

// respondTo checks the exact command before it sends the response.
func (e *fakeEngine) respondTo(command, response string) {
	e.t.Helper()
	line, err := bufio.NewReader(e.conn).ReadString(0)
	if err != nil {
		e.t.Errorf("engine read: %v", err)
		return
	}
	if line != command {
		e.t.Errorf("engine command = %q; want %q", line, command)
	}
	e.send(xmlProlog + withTx(response, txOf(line)))
}

// readCmd reads one command that xdbg sent (up to the NUL byte). It returns the
// command without the "-i <tx>" part, for example `property_get -d 0 -n "$a"`,
// and the transaction id, for the reply.
func (e *fakeEngine) readCmd() (cmd, tx string) {
	cmd, tx, err := e.readCmdErr()
	if err != nil && !isConnClosed(err) {
		e.t.Errorf("engine read: %v", err)
	}
	return cmd, tx
}

// readCmdErr is readCmd without the t.Errorf, so callers (and goroutines that
// may outlive the test) can inspect the error themselves.
func (e *fakeEngine) readCmdErr() (cmd, tx string, err error) {
	_ = e.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b []byte
	one := make([]byte, 1)
	for {
		if _, err = e.conn.Read(one); err != nil {
			return "", "", err
		}
		if one[0] == 0 {
			break
		}
		b = append(b, one[0])
	}
	parts := strings.SplitN(string(b), " ", 4) // name, "-i", tx, rest
	if len(parts) < 3 || parts[1] != "-i" {
		return string(b), "", fmt.Errorf("engine got a command without -i: %q", b)
	}
	cmd = parts[0]
	if len(parts) == 4 {
		cmd += " " + parts[3]
	}
	return cmd, parts[2], nil
}

// isConnClosed reports whether err is the expected result of the test (or the
// debugger) closing the engine connection, so a read helper must not call
// t.Errorf after the test has completed (which panics the whole binary).
func isConnClosed(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
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

func TestReadPacketNegativeLength(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.r = bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	go func() {
		_, _ = eng.conn.Write([]byte("-1\x00"))
	}()

	got, err := s.readPacket()
	if err == nil || !strings.Contains(err.Error(), "bad length -1") || got != "" {
		t.Fatalf("readPacket = %q, %v; want a negative length error", got, err)
	}
}

func TestReadPacketTooLarge(t *testing.T) {
	for _, length := range []string{"67108865", "9999999999", "999999999999999"} {
		t.Run(length, func(t *testing.T) {
			eng, conn := newPipe(t)
			s := newSession("/l", "/d")
			s.r = bufio.NewReader(conn)
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			go func() {
				_, _ = eng.conn.Write([]byte(length + "\x00"))
			}()

			got, err := s.readPacket()
			if err == nil || !strings.Contains(err.Error(), "max 67108864") || got != "" {
				t.Fatalf("readPacket = %q, %v; want an error that names the maximum length", got, err)
			}
		})
	}
}

func TestReadPacketEmptyLength(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.r = bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	go func() {
		_, _ = eng.conn.Write([]byte("\x00"))
	}()

	got, err := s.readPacket()
	if err == nil || !strings.Contains(err.Error(), "bad length") || got != "" {
		t.Fatalf("readPacket = %q, %v; want an empty length error", got, err)
	}
}

func TestReadPacketMissingTrailingNul(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packet  string
		wantErr string
	}{
		{"non-NUL byte", "3\x00abcX", "expected NUL"},
		{"EOF", "3\x00abc", "read trailing NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, conn := newPipe(t)
			s := newSession("/l", "/d")
			s.r = bufio.NewReader(conn)
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			go func() {
				_, _ = eng.conn.Write([]byte(tc.packet))
				_ = eng.conn.Close()
			}()

			got, err := s.readPacket()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
				t.Fatalf("readPacket = %q, %v; want %q", got, err, tc.wantErr)
			}
			if tc.name == "EOF" && !errors.Is(err, io.EOF) {
				t.Fatalf("error = %v, want a wrapped EOF", err)
			}
		})
	}
}

func TestRawLockedBadLengthClosesConn(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn, s.r)
	go func() {
		_, _ = bufio.NewReader(eng.conn).ReadString(0) // wait for eval
		_, _ = eng.conn.Write([]byte("-1\x00"))
	}()

	r, raw, err := s.cmd("eval", "-- x")
	if err == nil || !strings.Contains(err.Error(), "bad length -1") || r != nil || raw != "" {
		t.Fatalf("cmd = %+v, %q, %v; want a negative length error", r, raw, err)
	}
	if s.state != "no session" || s.conn != nil {
		t.Fatalf("state = %q, conn nil = %v", s.state, s.conn == nil)
	}
	_ = eng.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := eng.conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("engine read = %v, want EOF after the debugger closes the connection", err)
	}
}

func TestRawLockedMalformedResponse(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	s.startReader(conn, s.r)
	go func() {
		_, _ = bufio.NewReader(eng.conn).ReadString(0)                  // wait for eval
		eng.send(xmlProlog + `<response command="eval" status="break"`) // truncated XML
	}()

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
	s.startReader(conn, s.r)
	go func() {
		_, _ = bufio.NewReader(eng.conn).ReadString(0) // wait for run
		eng.send(xmlProlog + `<response command="run" transaction_id="1" status="break"/>`)
	}()

	r, _, err := s.cmd("run", "")
	if err != nil || r == nil || r.Status != "break" || s.state != "break" {
		t.Fatalf("got %+v, %v, state %q", r, err, s.state)
	}
}

func TestRawLockedSkipsStreamPackets(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn, s.r)
	go func() {
		eng.readCmd()
		eng.send(xmlProlog + `<stream type="stdout" encoding="base64">T1VUCg==</stream>`)
		eng.send(xmlProlog + `<stream type="stdout" encoding="base64">T1VUCg==</stream>`)
		eng.send(xmlProlog + `<response command="step_over" transaction_id="1" status="break" reason="ok"/>`)
	}()

	r, _, err := s.cmd("step_over", "")
	if err != nil || r == nil || r.Status != "break" || s.state != "break" {
		t.Fatalf("got %+v, %v, state %q", r, err, s.state)
	}
}

func TestRawLockedSkipsNotify(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn, s.r)
	go func() {
		eng.readCmd()
		eng.send(xmlProlog + `<notify name="breakpoint_resolved"/>`)
		eng.send(xmlProlog + `<response command="breakpoint_set" transaction_id="1" id="70001"/>`)
	}()

	r, _, err := s.cmd("breakpoint_set", "-t line -f file:///d/a.php -n 3")
	if err != nil || r == nil || r.ID != "70001" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestRawLockedSkipsUnsolicitedResponse(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn, s.r)
	go func() {
		eng.readCmd()
		eng.send(xmlProlog + `<response command="frobnicate" transaction_id="1"><error code="4"><message>unimplemented command</message></error></response>`)
		eng.readCmd()
		eng.send(xmlProlog + `<response status="stopping" reason="ok"/>`)
		eng.send(xmlProlog + `<response command="stack_get" transaction_id="2"><stack where="{main}" level="0" filename="file:///d/a.php" lineno="3"/></response>`)
	}()

	if _, _, err := s.cmd("frobnicate", ""); err == nil {
		t.Fatal("frobnicate = nil error, want the engine error")
	}
	r, _, err := s.cmd("stack_get", "")
	if err != nil || r == nil || len(r.Stacks) != 1 {
		t.Fatalf("got %+v, %v; want one stack frame", r, err)
	}
	if s.state == "stopping" {
		t.Fatalf("state = %q; an unsolicited response must not be adopted", s.state)
	}
}

func TestRawLockedSkipsWrongTransactionID(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn, s.r)
	go func() {
		eng.readCmd()
		eng.send(xmlProlog + `<response command="status" transaction_id="99" status="stopping"/>`)
		eng.send(xmlProlog + `<response command="run" transaction_id="1" status="break"/>`)
	}()

	r, _, err := s.cmd("run", "")
	if err != nil || r == nil || r.Status != "break" {
		t.Fatalf("got %+v, %v; want the real reply", r, err)
	}
	if s.state == "stopping" {
		t.Fatalf("state = %q; a wrong transaction_id must not be adopted", s.state)
	}
}

func TestRawLockedTooManySkippedPackets(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = "started"
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	s.startReader(conn, s.r)
	go func() {
		eng.readCmd()
		for i := 0; i <= maxSkippedPackets; i++ {
			eng.send(xmlProlog + `<stream type="stdout" encoding="base64">T1VUCg==</stream>`)
		}
	}()

	_, _, err := s.cmd("run", "")
	if err == nil || !strings.Contains(err.Error(), "no response with transaction_id 1") {
		t.Fatalf("err = %v; want the skipped-packet limit error", err)
	}
}

func TestRawLockedReturnsDBGpError(t *testing.T) {
	// Exercises the production command path (cmd): an engine <error> is
	// returned with its code and message, the state/location are still applied,
	// and the session stays usable.
	s, eng := newActivePipe(t)
	response := `<response command="run" transaction_id="1" status="break"><message filename="file:///d/a.php" lineno="9"/>` +
		`<error code="5"><message><![CDATA[ command is not available ]]></message></error></response>`
	go eng.respond(response, `<response command="stack_get" status="break"/>`)

	r, raw, err := s.cmd("run", "")
	if err == nil || err.Error() != "run error 5: command is not available" {
		t.Fatalf("cmd error = %v, want the engine code and message", err)
	}
	if r == nil || r.Error == nil || r.Error.Code != "5" || raw != xmlProlog+response {
		t.Fatalf("cmd = %+v, %q; want the parsed response and raw XML", r, raw)
	}
	if s.state != "break" || s.conn == nil || s.file != "/l/a.php" || s.line != 9 {
		t.Fatalf("state = %q, conn nil = %v, location = %s", s.state, s.conn == nil, s.location())
	}
	if _, err := s.Stack(); err != nil {
		t.Fatalf("the session must remain usable after an engine error: %v", err)
	}
}

func TestSetBreakpointEncodesPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		uri  string
	}{
		{"spaces", "my dir/a b.php", "file:///d/my%20dir/a%20b.php"},
		{"reserved characters", `my dir/100%#?"\.php`, "file:///d/my%20dir/100%25%23%3F%22%5C.php"},
		{"non-ASCII", "my dir/żółć.php", "file:///d/my%20dir/%C5%BC%C3%B3%C5%82%C4%87.php"},
	} {
		for _, queued := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/queued=%v", tc.name, queued), func(t *testing.T) {
				eng, conn := newPipe(t)
				s := newSession("/l", "/d")
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				tx := 1
				if queued {
					tx = 5
					if _, err := s.SetBreakpoint(tc.file, 3); err != nil {
						t.Fatal(err)
					}
				} else {
					s.conn, s.r, s.state = conn, bufio.NewReader(conn), "started"
					s.startReader(conn, s.r)
				}
				command := fmt.Sprintf("breakpoint_set -i %d -t line -f \"%s\" -n 3\x00", tx, tc.uri)
				go func() {
					if queued {
						eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
						eng.respond(
							`<response command="feature_set" success="1"/>`,
							`<response command="feature_set" success="1"/>`,
							`<response command="feature_set" success="1"/>`,
							featureAsyncResponse,
						)
					}
					eng.respondTo(command, `<response command="breakpoint_set" id="7"/>`)
				}()

				if queued {
					s.adopt(conn)
				} else if _, err := s.SetBreakpoint(tc.file, 3); err != nil {
					t.Fatal(err)
				}
				if len(s.pending) != 1 || s.pending[0].id != "7" || s.pending[0].file != "/d/"+tc.file {
					t.Fatalf("pending = %+v; want the original path and engine id 7", s.pending)
				}
			})
		}
	}
}

func TestSetBreakpointEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(`<response command="breakpoint_set"><error code="200"><message>breakpoint could not be set</message></error></response>`)

	text, err := s.SetBreakpoint("a.php", 3)
	if err == nil || err.Error() != "breakpoint_set error 200: breakpoint could not be set" || text != "" {
		t.Fatalf("SetBreakpoint = %q, %v; want the engine error", text, err)
	}
	if len(s.pending) != 0 {
		t.Fatalf("a rejected live breakpoint must not be stored, got %+v", s.pending)
	}
}

func TestBreakpointListNoSessionShowsQueued(t *testing.T) {
	s := newSession("/home/dev/app", "/var/www/app")
	for _, p := range []bp{{file: "src/Foo.php", line: 10}, {file: "src/Bar.php", line: 20}} {
		if _, err := s.SetBreakpoint(p.file, p.line); err != nil {
			t.Fatal(err)
		}
	}

	text, err := s.BreakpointList()
	want := "queued q1 /home/dev/app/src/Foo.php:10\nqueued q2 /home/dev/app/src/Bar.php:20\n"
	if err != nil || text != want {
		t.Fatalf("BreakpointList = %q, %v; want %q without an error", text, err, want)
	}
}

func TestBreakpointListNoSessionEmpty(t *testing.T) {
	s := newSession("/home/dev/app", "/var/www/app")

	text, err := s.BreakpointList()
	if err != nil || text != "(none)" {
		t.Fatalf("BreakpointList = %q, %v; want (none) without an error", text, err)
	}
}

func TestBreakpointListNoSessionRetainsRejected(t *testing.T) {
	s := newSession("/home/dev/app", "/var/www/app")
	s.pending = []bp{
		{file: "/var/www/app/a.php", line: 3, id: "1", qid: "q1"},
		{file: "/var/www/app/b.php", line: 5, qid: "q2", err: "breakpoint_set error 200: breakpoint could not be set"},
	}

	text, err := s.BreakpointList()
	want := "queued q1 /home/dev/app/a.php:3\n" +
		"rejected q2 /home/dev/app/b.php:5: breakpoint_set error 200: breakpoint could not be set\n"
	if err != nil || text != want {
		t.Fatalf("BreakpointList = %q, %v; want %q without an error", text, err, want)
	}
}

func TestBreakpointListWithSessionMergesQueued(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/home/dev/app", "/var/www/app")
	if _, err := s.SetBreakpoint("a.php", 3); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	go func() {
		eng.send(xmlProlog + `<init fileuri="file:///var/www/app/index.php"/>`)
		eng.respond(
			`<response command="feature_set" success="1"/>`,
			`<response command="feature_set" success="1"/>`,
			`<response command="feature_set" success="1"/>`,
			featureAsyncResponse,
			`<response command="breakpoint_set" id="1"/>`,
			`<response command="breakpoint_list"><breakpoint id="1" state="enabled" filename="file:///var/www/app/a.php" lineno="3"/></response>`,
		)
	}()

	s.adopt(conn)
	if s.conn == nil || s.pending[0].id != "1" {
		t.Fatalf("adopt: conn nil = %v, pending = %+v; want an applied breakpoint", s.conn == nil, s.pending)
	}
	s.pending = append(s.pending,
		bp{file: "/var/www/app/b.php", line: 5, qid: "q2"},
		bp{file: "/var/www/app/c.php", line: 7, qid: "q3", err: "breakpoint_set error 200: breakpoint could not be set"},
	)

	text, err := s.BreakpointList()
	want := "id=1 enabled /home/dev/app/a.php:3\nqueued q2 /home/dev/app/b.php:5\n" +
		"rejected q3 /home/dev/app/c.php:7: breakpoint_set error 200: breakpoint could not be set\n"
	if err != nil || text != want {
		t.Fatalf("BreakpointList = %q, %v; want %q without an error or duplicate", text, err, want)
	}
}

func TestBreakpointListWithSessionEmpty(t *testing.T) {
	for _, pending := range [][]bp{nil, {{file: "/d/a.php", line: 3, id: "1"}}} {
		s, eng := newActivePipe(t)
		s.pending = pending
		go eng.respond(`<response command="breakpoint_list"/>`)

		text, err := s.BreakpointList()
		if err != nil || text != "(none)" {
			t.Fatalf("BreakpointList with pending %+v = %q, %v; want (none) without an error", pending, text, err)
		}
	}
}

func TestBreakpointListEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	s.pending = []bp{{file: "/d/a.php", line: 3}}
	go eng.respond(`<response command="breakpoint_list"><error code="5"><message>command is not available</message></error></response>`)

	text, err := s.BreakpointList()
	if err == nil || err.Error() != "breakpoint_list error 5: command is not available" || text != "" {
		t.Fatalf("BreakpointList = %q, %v; want the engine error, not queued breakpoints", text, err)
	}
}

func TestBreakpointRemoveQueuedNoSession(t *testing.T) {
	s := newSession("/home/dev/app", "/var/www/app")
	first, err := s.SetBreakpoint("src/Foo.php", 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SetBreakpoint("src/Bar.php", 20)
	if err != nil {
		t.Fatal(err)
	}
	if first != "breakpoint queued q1 /var/www/app/src/Foo.php:10 (applied on next session)" ||
		second != "breakpoint queued q2 /var/www/app/src/Bar.php:20 (applied on next session)" {
		t.Errorf("SetBreakpoint replies = %q, %q; want distinct local handles", first, second)
	}

	text, err := s.BreakpointRemove("q1")
	if err != nil || text != "removed q1" {
		t.Fatalf("BreakpointRemove = %q, %v; want removed q1 without a session", text, err)
	}
	if !strings.Contains(s.Status(), "breakpoints=1") || len(s.pending) != 1 ||
		s.pending[0].file != "/var/www/app/src/Bar.php" || s.pending[0].line != 20 {
		t.Fatalf("Status = %q, pending = %+v; want only the second breakpoint", s.Status(), s.pending)
	}
	text, err = s.BreakpointList()
	if err != nil || text != "queued q2 /home/dev/app/src/Bar.php:20\n" {
		t.Fatalf("BreakpointList = %q, %v; want the remaining handle and host path", text, err)
	}
}

func TestBreakpointRemoveEmptyIDNoStateChange(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%v", active), func(t *testing.T) {
			s := newSession("/l", "/d")
			for _, file := range []string{"a.php", "b.php"} {
				if _, err := s.SetBreakpoint(file, 3); err != nil {
					t.Fatal(err)
				}
			}
			if active {
				eng, conn := newPipe(t)
				s.conn, s.r, s.state = conn, bufio.NewReader(conn), "started"
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				go eng.respond(`<response command="breakpoint_remove"/>`)
			}
			before, status, tx := slices.Clone(s.pending), s.Status(), s.tx

			text, err := s.BreakpointRemove("")
			if err == nil || err.Error() != "id required" || text != "" {
				t.Errorf("BreakpointRemove = %q, %v; want an id required error", text, err)
			}
			if !slices.Equal(s.pending, before) || s.Status() != status || s.tx != tx {
				t.Fatalf("Status = %q, pending = %+v, tx = %d; empty id must not change state", s.Status(), s.pending, s.tx)
			}
		})
	}
}

func TestBreakpointRemoveRejectsInvalidID(t *testing.T) {
	for _, id := range []string{"abc", "q99", "1 -d 2", "1\x00stop", "-1", "+1", "1.0", " 1", "1 ", "١"} {
		for _, active := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/active=%v", id, active), func(t *testing.T) {
				s := newSession("/l", "/d")
				if active {
					s, _ = newActivePipe(t)
				}
				s.pending = []bp{{file: "/d/a.php", line: 3, id: "1001", qid: "q1"}}
				before, status := slices.Clone(s.pending), s.Status()

				text, err := s.BreakpointRemove(id)
				if err == nil || !strings.Contains(err.Error(), "numeric") || text != "" {
					t.Fatalf("BreakpointRemove = %q, %v; want a numeric id error", text, err)
				}
				if !slices.Equal(s.pending, before) || s.Status() != status || s.tx != 0 {
					t.Fatalf("Status = %q, pending = %+v, tx = %d; an invalid id must not change state", s.Status(), s.pending, s.tx)
				}
			})
		}
	}
}

func TestBreakpointRemoveRejectsInvalidEngineID(t *testing.T) {
	s, _ := newActivePipe(t)
	s.pending = []bp{{file: "/d/a.php", line: 3, id: "1001 -d 2", qid: "q1"}}
	before := slices.Clone(s.pending)

	text, err := s.BreakpointRemove("q1")
	if err == nil || !strings.Contains(err.Error(), "numeric") || text != "" || !slices.Equal(s.pending, before) || s.tx != 0 {
		t.Fatalf("BreakpointRemove = %q, %v; pending = %+v, tx = %d; want rejection of the invalid engine id", text, err, s.pending, s.tx)
	}
}

func TestBreakpointRemoveNumericID(t *testing.T) {
	for _, id := range []string{"0", "001", "184467440737095516160"} {
		t.Run(id, func(t *testing.T) {
			s, eng := newActivePipe(t)
			go eng.respondTo("breakpoint_remove -i 1 -d "+id+"\x00", `<response command="breakpoint_remove"/>`)

			text, err := s.BreakpointRemove(id)
			if err != nil || text != "removed "+id {
				t.Fatalf("BreakpointRemove = %q, %v; want removal of the numeric id", text, err)
			}
		})
	}
}

func TestBreakpointRemoveEngineErrorKeepsState(t *testing.T) {
	for _, id := range []string{"1001", "q1", "9001"} {
		t.Run(id, func(t *testing.T) {
			engineID := id
			if id == "q1" {
				engineID = "1001"
			}
			eng, conn := newPipe(t)
			s := newSession("/l", "/d")
			if _, err := s.SetBreakpoint("a.php", 3); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			command := make(chan string, 1)
			go func() {
				eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
				eng.respond(
					`<response command="feature_set" success="1"/>`,
					`<response command="feature_set" success="1"/>`,
					`<response command="feature_set" success="1"/>`,
					featureAsyncResponse,
					`<response command="breakpoint_set" id="1001"/>`,
				)
				line, err := bufio.NewReader(eng.conn).ReadString(0)
				if err != nil {
					t.Errorf("engine read: %v", err)
					return
				}
				command <- line
				eng.send(xmlProlog + withTx(`<response command="breakpoint_remove"><error code="205"><message>no such breakpoint</message></error></response>`, txOf(line)))
				eng.respond(`<response command="breakpoint_list"><breakpoint id="1001" state="enabled" filename="file:///d/a.php" lineno="3"/></response>`)
			}()
			s.adopt(conn)
			if len(s.pending) != 1 || s.pending[0].id != "1001" || s.conn == nil {
				t.Fatalf("adopt: conn nil = %v, pending = %+v; want an applied breakpoint", s.conn == nil, s.pending)
			}
			before, status := slices.Clone(s.pending), s.Status()

			text, err := s.BreakpointRemove(id)
			if err == nil || err.Error() != "breakpoint_remove error 205: no such breakpoint" || text != "" {
				t.Fatalf("BreakpointRemove = %q, %v; want the engine error", text, err)
			}
			if !slices.Equal(s.pending, before) || s.Status() != status {
				t.Fatalf("Status = %q, pending = %+v; an engine error must retain the local breakpoint", s.Status(), s.pending)
			}
			select {
			case line := <-command:
				if line != "breakpoint_remove -i 6 -d "+engineID+"\x00" {
					t.Fatalf("engine command = %q; want engine id %s", line, engineID)
				}
			default:
				t.Fatal("the engine did not receive breakpoint_remove")
			}
			text, err = s.BreakpointList()
			if err != nil || text != "id=1001 enabled /l/a.php:3\n" {
				t.Fatalf("BreakpointList = %q, %v; want the retained engine breakpoint", text, err)
			}
		})
	}
}

func TestBreakpointRemoveWireErrorKeepsState(t *testing.T) {
	for _, failure := range []string{"write", "read", "XML"} {
		t.Run(failure, func(t *testing.T) {
			s, eng := newActivePipe(t)
			s.pending = []bp{{file: "/d/a.php", line: 3, id: "1001", qid: "q1"}}
			before := slices.Clone(s.pending)
			switch failure {
			case "write":
				_ = eng.conn.Close()
			case "read":
				go func() {
					if _, err := bufio.NewReader(eng.conn).ReadString(0); err != nil {
						return
					}
					_ = eng.conn.Close()
				}()
			case "XML":
				go eng.respond(`<response command="breakpoint_remove"`)
			}

			text, err := s.BreakpointRemove("q1")
			if err == nil || text != "" || !slices.Equal(s.pending, before) {
				t.Fatalf("BreakpointRemove = %q, %v; pending = %+v; a wire error must retain the local breakpoint", text, err, s.pending)
			}
		})
	}
}

func TestBreakpointRemoveModes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       string
		engineID string
		active   bool
		applied  bool
		rejected bool
		wantErr  string
		wantKept bool
	}{
		{name: "queued with session", id: "q1", active: true},
		{name: "rejected with session", id: "q1", active: true, rejected: true},
		{name: "applied by engine id", id: "1001", engineID: "1001", active: true, applied: true},
		{name: "applied by handle", id: "q1", engineID: "1001", active: true, applied: true},
		{name: "applied without session by engine id", id: "1001", applied: true},
		{name: "applied without session by handle", id: "q1", applied: true},
		{name: "unknown with session", id: "9001", engineID: "9001", active: true, applied: true, wantKept: true},
		{name: "unknown without session", id: "9001", applied: true, wantErr: "no active session", wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession("/l", "/d")
			if _, err := s.SetBreakpoint("a.php", 3); err != nil {
				t.Fatal(err)
			}
			if tc.applied {
				s.pending[0].id = "1001"
			}
			if tc.rejected {
				s.pending[0].err = "breakpoint_set error 200: breakpoint could not be set"
			}
			command := make(chan string, 1)
			if tc.active {
				eng, conn := newPipe(t)
				s.conn, s.r, s.state = conn, bufio.NewReader(conn), "started"
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				s.startReader(conn, s.r)
				go func() {
					line, err := bufio.NewReader(eng.conn).ReadString(0)
					if err != nil {
						return
					}
					command <- line
					eng.send(xmlProlog + withTx(`<response command="breakpoint_remove"/>`, txOf(line)))
				}()
			}
			var wantPending []bp
			if tc.wantKept {
				wantPending = slices.Clone(s.pending)
			}

			text, err := s.BreakpointRemove(tc.id)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr || text != "" {
					t.Fatalf("BreakpointRemove = %q, %v; want %q", text, err, tc.wantErr)
				}
			} else if err != nil || text != "removed "+tc.id {
				t.Fatalf("BreakpointRemove = %q, %v; want removed %s", text, err, tc.id)
			}
			if !slices.Equal(s.pending, wantPending) {
				t.Fatalf("pending = %+v; want %+v", s.pending, wantPending)
			}
			if tc.engineID == "" {
				if s.tx != 0 {
					t.Fatalf("tx = %d; local removal must not send an engine command", s.tx)
				}
			} else {
				select {
				case line := <-command:
					if line != "breakpoint_remove -i 1 -d "+tc.engineID+"\x00" {
						t.Fatalf("engine command = %q; want engine id %s", line, tc.engineID)
					}
				default:
					t.Fatal("the engine did not receive breakpoint_remove")
				}
			}
		})
	}
}

func TestBreakpointHandlesNotReused(t *testing.T) {
	s := newSession("/l", "/d")
	for _, file := range []string{"a.php", "b.php"} {
		if _, err := s.SetBreakpoint(file, 3); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BreakpointRemove("q1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBreakpoint("c.php", 3); err != nil {
		t.Fatal(err)
	}
	text, err := s.BreakpointList()
	if err != nil || text != "queued q2 /l/b.php:3\nqueued q3 /l/c.php:3\n" {
		t.Fatalf("BreakpointList = %q, %v; handles must stay stable after removal", text, err)
	}
	if _, err := s.BreakpointClearAll(); err != nil {
		t.Fatal(err)
	}
	text, err = s.SetBreakpoint("d.php", 3)
	if err != nil || text != "breakpoint queued q4 /d/d.php:3 (applied on next session)" {
		t.Fatalf("SetBreakpoint = %q, %v; a cleared handle must not be reused", text, err)
	}
}

func TestBreakpointHandleAcrossSessions(t *testing.T) {
	s := newSession("/l", "/d")
	if _, err := s.SetBreakpoint("a.php", 3); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"1001", "2001"} {
		eng, conn := newPipe(t)
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		command := make(chan string, 1)
		go func() {
			eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
			eng.respond(
				`<response command="feature_set" success="1"/>`,
				`<response command="feature_set" success="1"/>`,
				`<response command="feature_set" success="1"/>`,
				featureAsyncResponse,
				`<response command="breakpoint_set" id="`+id+`"/>`,
			)
			if i == 0 {
				eng.respond(`<response command="detach"/>`)
				return
			}
			line, err := bufio.NewReader(eng.conn).ReadString(0)
			if err != nil {
				t.Errorf("engine read: %v", err)
				return
			}
			command <- line
			eng.send(xmlProlog + withTx(`<response command="breakpoint_remove"/>`, txOf(line)))
		}()
		s.adopt(conn)
		if len(s.pending) != 1 || s.pending[0].qid != "q1" || s.pending[0].id != id {
			t.Fatalf("pending = %+v; want stable handle q1 and current engine id %s", s.pending, id)
		}
		if i == 0 {
			if _, err := s.Detach(); err != nil {
				t.Fatal(err)
			}
			text, err := s.BreakpointList()
			if err != nil || text != "queued q1 /l/a.php:3\n" {
				t.Fatalf("BreakpointList = %q, %v; want the same handle after detach", text, err)
			}
			continue
		}

		text, err := s.BreakpointRemove("q1")
		if err != nil || text != "removed q1" || len(s.pending) != 0 {
			t.Fatalf("BreakpointRemove = %q, %v; pending = %+v; want removal by the original handle", text, err, s.pending)
		}
		select {
		case line := <-command:
			if line != "breakpoint_remove -i 6 -d 2001\x00" {
				t.Fatalf("engine command = %q; want the current engine id 2001", line)
			}
		default:
			t.Fatal("the engine did not receive breakpoint_remove")
		}
	}
}

func TestBreakpointClearAllBestEffort(t *testing.T) {
	s, eng := newActivePipe(t)
	s.pending = []bp{
		{file: "/d/a.php", line: 3, qid: "q1"},
		{file: "/d/b.php", line: 4, id: "1001", qid: "q2"},
		{file: "/d/c.php", line: 5, id: "1002", qid: "q3"},
		{file: "/d/d.php", line: 6, qid: "q4", err: "breakpoint_set error 200: breakpoint could not be set"},
	}
	go eng.respond(
		`<response command="breakpoint_remove"><error code="205"><message>no such breakpoint</message></error></response>`,
		`<response command="breakpoint_remove"/>`,
	)

	text, err := s.BreakpointClearAll()
	if err != nil || text != "cleared 4 breakpoint(s)" || len(s.pending) != 0 || s.tx != 2 {
		t.Fatalf("BreakpointClearAll = %q, %v; pending = %+v, tx = %d; want best-effort removal of all entries", text, err, s.pending, s.tx)
	}
}

func TestBreakpointClearAllRejectsInvalidEngineID(t *testing.T) {
	s, _ := newActivePipe(t)
	s.pending = []bp{
		{file: "/d/a.php", line: 3, id: "1001", qid: "q1"},
		{file: "/d/b.php", line: 4, id: "1002\x00stop", qid: "q2"},
	}
	before := slices.Clone(s.pending)

	text, err := s.BreakpointClearAll()
	if err == nil || !strings.Contains(err.Error(), "numeric") || text != "" || !slices.Equal(s.pending, before) || s.tx != 0 {
		t.Fatalf("BreakpointClearAll = %q, %v; pending = %+v, tx = %d; want rejection before any state change", text, err, s.pending, s.tx)
	}
}

func TestPropertySetQuotesName(t *testing.T) {
	for _, tc := range []struct{ name, arg string }{
		{"$x", `"$x"`},
		{"$arr['a b']", `"$arr['a b']"`},
		{`$arr["k"]`, `"$arr[\"k\"]"`},
		{`$arr['a\b']`, `"$arr['a\\b']"`},
		{`$arr["a\b"]`, `"$arr[\"a\\b\"]"`},
		{"$arr['żółć']", `"$arr['żółć']"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, eng := newActivePipe(t)
			go eng.respondTo("property_set -i 1 -n "+tc.arg+" -- OTk=\x00", `<response command="property_set" success="1"/>`)

			text, err := s.PropertySet(tc.name, "99")
			if err != nil || text != tc.name+" = 99" {
				t.Fatalf("PropertySet = %q, %v; want a successful assignment", text, err)
			}
		})
	}
}

func TestPropertyNamesRejectNUL(t *testing.T) {
	for _, method := range []string{"get", "set"} {
		t.Run(method, func(t *testing.T) {
			s, _ := newActivePipe(t)
			var text string
			var err error
			if method == "get" {
				text, err = s.PropertyGet("$x\x00stop", 0)
			} else {
				text, err = s.PropertySet("$x\x00stop", "99")
			}
			if err == nil || err.Error() != "name must not contain NUL" || text != "" || s.tx != 0 || s.state != "started" {
				t.Fatalf("Property%s = %q, %v; tx = %d, state = %q; want a NUL error without a command", method, text, err, s.tx, s.state)
			}
		})
	}
}

func TestPropertySetEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(`<response command="property_set" success="0"><error code="300"><message>can not get property</message></error></response>`)

	text, err := s.PropertySet("$x", "42")
	if err == nil || err.Error() != "property_set error 300: can not get property" || text != "" {
		t.Fatalf("PropertySet = %q, %v; want the engine error", text, err)
	}
}

func TestPropertySetSuccessZero(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(`<response command="property_set" success="0"/>`)

	text, err := s.PropertySet("$x", "42")
	if err == nil || !strings.Contains(err.Error(), "property_set") || !strings.Contains(err.Error(), `success="0"`) || text != "" {
		t.Fatalf("PropertySet = %q, %v; want an error for success=0", text, err)
	}
}

func TestPropertySetSuccess(t *testing.T) {
	for _, attrs := range []string{` success="1"`, ""} {
		t.Run(attrs, func(t *testing.T) {
			s, eng := newActivePipe(t)
			go eng.respond(`<response command="property_set"` + attrs + `/>`)

			text, err := s.PropertySet("$x", "42")
			if err != nil || text != "$x = 42" {
				t.Fatalf("PropertySet = %q, %v; want a successful assignment", text, err)
			}
		})
	}
}

func TestPropertyGetQuotesName(t *testing.T) {
	for _, tc := range []struct{ name, arg string }{
		{"$x", `"$x"`},
		{"$arr['a b']", `"$arr['a b']"`},
		{`$arr["k"]`, `"$arr[\"k\"]"`},
		{`$arr['a\b']`, `"$arr['a\\b']"`},
		{`$arr["a\b"]`, `"$arr[\"a\\b\"]"`},
		{"$arr['żółć']", `"$arr['żółć']"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, eng := newActivePipe(t)
			go eng.respondTo("property_get -i 1 -d 2 -n "+tc.arg+"\x00", `<response command="property_get"><property type="int">5</property></response>`)

			text, err := s.PropertyGet(tc.name, 2)
			if err != nil || text != "5" {
				t.Fatalf("PropertyGet = %q, %v; want the property value", text, err)
			}
		})
	}
}

func TestPropertyGetEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(`<response command="property_get"><error code="300"><message>can not get property</message></error></response>`)

	text, err := s.PropertyGet("$missing", 0)
	if err == nil || err.Error() != "property_get error 300: can not get property" || text != "" {
		t.Fatalf("PropertyGet = %q, %v; want the engine error, not (not found)", text, err)
	}
}

func TestPropertyGetNoProperty(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(`<response command="property_get"/>`)

	text, err := s.PropertyGet("$missing", 0)
	if err != nil || text != "(not found)" {
		t.Fatalf("PropertyGet = %q, %v; want (not found) without an engine error", text, err)
	}
}

func TestEvalEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(`<response command="eval"><error code="206"><message><![CDATA[error evaluating code]]></message></error></response>`)

	text, err := s.Eval("invalid expression")
	if err == nil || err.Error() != "eval error 206: error evaluating code" || text != "" {
		t.Fatalf("Eval = %q, %v; want the existing eval error text", text, err)
	}
}

func TestRawEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	response := `<response command="property_get" transaction_id="1"><error code="300"><message>can not get property</message></error></response>`
	go eng.respond(response)

	raw, err := s.Raw("property_get -n $missing")
	if err == nil || err.Error() != "property_get error 300: can not get property" || raw != xmlProlog+response {
		t.Fatalf("Raw = %q, %v; want raw XML and the engine error", raw, err)
	}
}

func TestAdoptRejectedPendingBreakpoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner string
	}{
		{"empty engine list", ""},
		{"accepted and rejected", `<breakpoint id="7" state="enabled" filename="file:///d/b.php" lineno="4"/>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, conn := newPipe(t)
			s := newSession("/l", "/d")
			if _, err := s.SetBreakpoint("a.php", 3); err != nil {
				t.Fatal(err)
			}
			responses := []string{
				`<response command="feature_set" success="1"/>`,
				`<response command="feature_set"><error code="3"><message>invalid arguments</message></error></response>`,
				`<response command="feature_set" success="1"/>`,
				featureAsyncResponse,
				`<response command="breakpoint_set"><error code="200"><message>breakpoint could not be set</message></error></response>`,
			}
			if tc.inner != "" {
				if _, err := s.SetBreakpoint("b.php", 4); err != nil {
					t.Fatal(err)
				}
				responses = append(responses, `<response command="breakpoint_set" id="7"/>`)
			}
			responses = append(responses, `<response command="breakpoint_list">`+tc.inner+`</response>`)
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			go func() {
				eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
				eng.respond(responses...)
			}()
			var logs bytes.Buffer
			oldWriter := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(oldWriter) })
			ready := s.ready

			s.adopt(conn)
			log.SetOutput(oldWriter)

			if s.state != "started" || s.conn == nil || s.handshakeError() != nil {
				t.Fatalf("state = %q, conn nil = %v, handshake error = %v", s.state, s.conn == nil, s.handshakeError())
			}
			if s.pending[0].id != "" {
				t.Fatalf("a rejected breakpoint must have no engine id, got %q", s.pending[0].id)
			}
			select {
			case <-ready:
			default:
				t.Fatal("adopt must wake the waiters after a rejected breakpoint")
			}
			if !strings.Contains(logs.String(), "breakpoint /d/a.php:3 rejected: breakpoint_set error 200: breakpoint could not be set") {
				t.Fatalf("the rejected breakpoint was not logged: %q", logs.String())
			}
			text, err := s.BreakpointList()
			if err != nil || !strings.Contains(text, "rejected q1 /l/a.php:3") || !strings.Contains(text, "200: breakpoint could not be set") || strings.Contains(text, "queued q1 /l/a.php:3") {
				t.Fatalf("BreakpointList = %q, %v; want the rejected breakpoint and its error", text, err)
			}
			if tc.inner != "" && !strings.Contains(text, "id=7 enabled /l/b.php:4") {
				t.Fatalf("BreakpointList must retain accepted breakpoints, got %q", text)
			}
		})
	}
}

func TestAdoptNegativeLengthInit(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	ready := s.ready
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	go func() {
		_, _ = eng.conn.Write([]byte("-1\x00"))
	}()

	s.adopt(conn)

	if s.state != "no session" || s.conn != nil {
		t.Fatalf("state = %q, conn nil = %v", s.state, s.conn == nil)
	}
	select {
	case <-ready:
	default:
		t.Fatal("a failed handshake must wake the waiters")
	}
	if err := s.handshakeError(); err == nil || !strings.Contains(err.Error(), "read init") || !strings.Contains(err.Error(), "bad length -1") {
		t.Fatalf("handshakeError = %v, want a read init error for a negative length", err)
	}
	_ = eng.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := eng.conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("engine read = %v, want EOF after the debugger closes the connection", err)
	}
}

func TestAdoptMalformedInit(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	ready := s.ready
	eng.drain()
	go eng.send(xmlProlog + `<init fileuri="file:///d/index.php"`) // truncated XML
	// Without the fix adopt goes on to negotiate features; the deadline makes that fail fast.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	s.adopt(conn)

	if s.state != "no session" {
		t.Fatalf("state = %q, want %q", s.state, "no session")
	}
	if s.conn != nil {
		t.Fatal("the connection must be dropped after a malformed init packet")
	}
	select {
	case <-ready:
	default:
		t.Fatal("a failed handshake must wake the waiters")
	}
	if err := s.handshakeError(); err == nil || !strings.Contains(err.Error(), "parse init") {
		t.Fatalf("handshakeError = %v, want a parse init error", err)
	}
}

func TestAdoptTruncatedInitPacket(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	ready := s.ready
	go func() {
		// Announce 100 bytes, deliver 3, then hang up.
		_, _ = eng.conn.Write([]byte("100\x00abc"))
		_ = eng.conn.Close()
	}()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	s.adopt(conn)

	select {
	case <-ready:
	default:
		t.Fatal("a failed handshake must wake the waiters")
	}
	if err := s.handshakeError(); err == nil || !strings.Contains(err.Error(), "read init") {
		t.Fatalf("handshakeError = %v, want a read init error", err)
	}
	if s.state != "no session" || s.conn != nil {
		t.Fatalf("state = %q, conn nil = %v", s.state, s.conn == nil)
	}
}

func TestListenWaitFailedHandshake(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	s := newSession("/l", "/d")
	s.dbgAddr = addr
	go func() {
		for i := 0; i < 100; i++ { // wait for the listener, then send a truncated init
			if c, err := net.Dial("tcp", addr); err == nil {
				_, _ = c.Write([]byte(dbgpPacket(xmlProlog + `<init fileuri="file:///d/index.php"`)))
				t.Cleanup(func() { _ = c.Close() })
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	start := time.Now()
	_, err = s.ListenWait(10 * time.Second)

	if err == nil || !strings.Contains(err.Error(), "handshake failed") {
		t.Fatalf("ListenWait error = %v, want a handshake error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("ListenWait took %s, it must not wait for the timeout", time.Since(start))
	}
}

// TestOpenOnceClosesListenerAfterFirstAccept verifies that openOnce closes the
// DBGp port as soon as the first Xdebug connection is accepted, so a second
// connection is refused at once instead of hanging in the listen backlog while
// adopt() runs the script to completion (no breakpoints).
func TestOpenOnceClosesListenerAfterFirstAccept(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = "127.0.0.1:0"
	if _, err := s.openOnce(5*time.Second, 0); err != nil {
		t.Fatalf("openOnce = %v, want nil", err)
	}

	// adopt replaces s.ready when it finishes, so save the current channel now.
	s.mu.Lock()
	addr := s.ln.Addr().String()
	ready := s.ready
	s.mu.Unlock()

	// Client 1: complete the DBGp handshake up to the run command.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("engine dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	eng := &fakeEngine{t: t, conn: conn}

	eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
	for i := 0; i < 3; i++ {
		line, err := r.ReadString(0)
		if err != nil {
			t.Fatalf("read feature_set: %v", err)
		}
		eng.send(xmlProlog + withTx(`<response command="feature_set" success="1"/>`, txOf(line)))
	}
	asyncLine, err := r.ReadString(0)
	if err != nil || !strings.HasPrefix(asyncLine, "feature_get -i ") {
		t.Fatalf("feature_get command = %q, %v; want a feature_get command", asyncLine, err)
	}
	eng.send(xmlProlog + withTx(featureAsyncResponse, txOf(asyncLine)))
	runLine, err := r.ReadString(0)
	if err != nil || !strings.HasPrefix(runLine, "run -i ") {
		t.Fatalf("run command = %q, %v; want a run command", runLine, err)
	}

	// Client 2: the listener must be closed after the first accept, so this is
	// refused instead of completing the handshake in the kernel backlog.
	if c2, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = c2.Close()
		t.Fatal("a second connection succeeded; the listener must be closed after the first accept")
	}

	// Finish the first session: run -> stopping, then stop -> stopped.
	eng.send(xmlProlog + withTx(`<response command="run" status="stopping"/>`, txOf(runLine)))
	stopLine, err := r.ReadString(0)
	if err != nil || !strings.HasPrefix(stopLine, "stop -i ") {
		t.Fatalf("stop command = %q, %v; want a stop command", stopLine, err)
	}
	eng.send(xmlProlog + withTx(`<response command="stop" status="stopped"/>`, txOf(stopLine)))

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("adopt did not finish within 2s")
	}
	if s.state != "no session" {
		t.Fatalf("state = %q, want %q", s.state, "no session")
	}
}

// freeAddr returns a currently free 127.0.0.1 address for the DBGp listener.
func freeAddr(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()
	return addr
}

// dialEngine waits for the debugger's listener at addr and returns a connection
// to it, closing the connection at test cleanup.
func dialEngine(t *testing.T, addr string) net.Conn {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			t.Cleanup(func() { _ = c.Close() })
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("engine could not connect to %s", addr)
	return nil
}

// slowEngine connects at once, waits delay (past the caller's accept timeout),
// then completes the DBGp handshake, applying the given number of queued
// breakpoints.
func slowEngine(t *testing.T, addr string, delay time.Duration, breakpoints int) {
	t.Helper()
	conn := dialEngine(t, addr)
	time.Sleep(delay)
	eng := &fakeEngine{t: t, conn: conn}
	eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
	responses := []string{
		`<response command="feature_set" success="1"/>`,
		`<response command="feature_set" success="1"/>`,
		`<response command="feature_set" success="1"/>`,
		featureAsyncResponse,
	}
	for i := 0; i < breakpoints; i++ {
		responses = append(responses, `<response command="breakpoint_set" id="7"/>`)
	}
	eng.respond(responses...)
}

// stuckEngine connects and never sends <init>, holding the connection open
// until hold is closed.
func stuckEngine(t *testing.T, addr string, hold <-chan struct{}) {
	t.Helper()
	_ = dialEngine(t, addr)
	<-hold
}

// assertNoOrphan checks that a timeout error did not leave an active session,
// and that a new listen is not rejected with "debug session already active".
func assertNoOrphan(t *testing.T, s *session, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if got := s.Status(); !strings.Contains(got, "state=no session") {
		t.Fatalf("error %v left session %q; want no session", err, got)
	}
	if _, lerr := s.ListenWait(100 * time.Millisecond); lerr != nil && strings.Contains(lerr.Error(), "already active") {
		t.Fatalf("next ListenWait = %v; want no 'already active' error", lerr)
	}
}

func TestListenWaitSlowHandshakeAfterTimeout(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = freeAddr(t)
	if _, err := s.SetBreakpoint("a.php", 3); err != nil {
		t.Fatal(err)
	}
	// The engine connects at once but finishes the handshake after the caller's
	// accept timeout. The caller must keep waiting for the handshake instead of
	// reporting "no engine connected" and leaving an orphan paused session.
	go slowEngine(t, s.dbgAddr, 500*time.Millisecond, 1)

	out, err := s.ListenWait(300 * time.Millisecond)
	if err != nil {
		// Option B: the caller gave up; it must not leave a session behind.
		assertNoOrphan(t, s, err)
		return
	}
	// Option A: the handshake finished; the client knows about the session.
	if !strings.Contains(out, "state=started") {
		t.Fatalf("ListenWait = %q, want an adopted paused session", out)
	}
}

func TestListenWaitStuckEngine(t *testing.T) {
	oldTimeout, oldGrace := handshakeTimeout, handshakeGrace
	handshakeTimeout, handshakeGrace = 200*time.Millisecond, 0
	t.Cleanup(func() { handshakeTimeout, handshakeGrace = oldTimeout, oldGrace })

	s := newSession("/l", "/d")
	s.dbgAddr = freeAddr(t)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	go stuckEngine(t, s.dbgAddr, hold)

	start := time.Now()
	_, err := s.ListenWait(300 * time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ListenWait = nil error, want a handshake timeout")
	}
	if elapsed > 300*time.Millisecond+handshakeTimeout+2*time.Second {
		t.Fatalf("ListenWait took %s; a stuck engine must not block forever", elapsed)
	}
	assertNoOrphan(t, s, err)
}

func TestRunCommandSlowHandshakeAfterTimeout(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = freeAddr(t)
	s.containerExec = "true"
	if _, err := s.SetBreakpoint("a.php", 3); err != nil {
		t.Fatal(err)
	}
	go slowEngine(t, s.dbgAddr, 500*time.Millisecond, 1)

	text, err := s.RunCommand("noop", 300*time.Millisecond)
	if err != nil {
		assertNoOrphan(t, s, err)
		return
	}
	if !strings.Contains(text, "paused") || !strings.Contains(s.Status(), "state=started") {
		t.Fatalf("RunCommand = %q, status %q; want an adopted paused session", text, s.Status())
	}
}

func TestRunCommandStuckEngine(t *testing.T) {
	oldTimeout, oldGrace := handshakeTimeout, handshakeGrace
	handshakeTimeout, handshakeGrace = 200*time.Millisecond, 0
	t.Cleanup(func() { handshakeTimeout, handshakeGrace = oldTimeout, oldGrace })

	s := newSession("/l", "/d")
	s.dbgAddr = freeAddr(t)
	s.containerExec = "true"
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	go stuckEngine(t, s.dbgAddr, hold)

	start := time.Now()
	_, err := s.RunCommand("noop", 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunCommand = nil error, want a handshake timeout")
	}
	if elapsed > 300*time.Millisecond+handshakeTimeout+2*time.Second {
		t.Fatalf("RunCommand took %s; a stuck engine must not block forever", elapsed)
	}
	assertNoOrphan(t, s, err)
}

func TestAdoptWellFormedInit(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.pending = []bp{{file: "/d/a.php", line: 3}}
	go func() {
		eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
		// Answer every command (3x feature_set, feature_get, breakpoint_set) with a generic response.
		for i := 0; i < 5; i++ {
			_, tx := eng.readCmd()
			eng.send(xmlProlog + `<response command="x" transaction_id="` + tx + `" id="7"/>`)
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

func TestToContainer(t *testing.T) {
	for name, tc := range map[string]struct{ local, docker, in, want string }{
		"host absolute":                      {"/home/dev/app", "/var/www/app", "/home/dev/app/src/A.php", "/var/www/app/src/A.php"},
		"project relative":                   {"/home/dev/app", "/var/www/app", "src/A.php", "/var/www/app/src/A.php"},
		"other absolute path":                {"/home/dev/app", "/var/www/app", "/src/A.php", "/src/A.php"},
		"exact local root":                   {"/home/dev/app", "/var/www/app", "/home/dev/app", "/var/www/app"},
		"already container path":             {"/home/dev/app", "/var/www/app", "/var/www/app/src/A.php", "/var/www/app/src/A.php"},
		"exact docker root":                  {"/home/dev/app", "/var/www/app", "/var/www/app", "/var/www/app"},
		"other absolute file":                {"/home/dev/app", "/var/www/app", "/etc/php.ini", "/etc/php.ini"},
		"nested relative":                    {"/home/dev/app", "/var/www/app", "src/Foo/Bar.php", "/var/www/app/src/Foo/Bar.php"},
		"sibling of local root":              {"/home/dev/app", "/var/www/app", "/home/dev/application/x.php", "/home/dev/application/x.php"},
		"host dot before root":               {"/home/dev/app", "/var/www/app", "/home/dev/./app/src/A.php", "/var/www/app/src/A.php"},
		"host parent before root":            {"/home/dev/app", "/var/www/app", "/home/dev/work/../app/src/A.php", "/var/www/app/src/A.php"},
		"host parent inside root":            {"/home/dev/app", "/var/www/app", "/home/dev/app/src/../A.php", "/var/www/app/A.php"},
		"host parent leaves root":            {"/home/dev/app", "/var/www/app", "/home/dev/app/../application/x.php", "/home/dev/app/../application/x.php"},
		"host repeated separators":           {"/home/dev/app", "/var/www/app", "/home//dev/app/src//A.php", "/var/www/app/src/A.php"},
		"roots and host repeated separators": {"/home/dev//app", "/var//www/app", "/home/dev//app/src/A.php", "/var/www/app/src/A.php"},
		"normalized exact local root":        {"/home/dev/app", "/var/www/app", "/home/dev/./app/", "/var/www/app"},
		"unclean container path unchanged":   {"/home/dev/app", "/var/www/app", "/var//www/app/src/../A.php", "/var//www/app/src/../A.php"},
		"empty docker root relative":         {"/home/dev/app", "", "src/A.php", "/home/dev/app/src/A.php"},
		"empty docker root absolute":         {"/home/dev/app", "", "/home/dev/app/src/A.php", "/home/dev/app/src/A.php"},
		"empty docker root unclean absolute": {"/home/dev/app", "", "/home//dev/app/../application/x.php", "/home//dev/app/../application/x.php"},
		"empty docker root outside":          {"/home/dev/app", "", "/usr/share/php/lib.php", "/usr/share/php/lib.php"},
		"relative dot segments":              {"/home/dev/app", "/var/www/app", "./src/../src/A.php", "/var/www/app/src/A.php"},
		"local filesystem root":              {"/", "/var/www/app", "/src/A.php", "/var/www/app/src/A.php"},
		"exact local filesystem root":        {"/", "/var/www/app", "/", "/var/www/app"},
		"docker filesystem root":             {"/home/dev/app", "/", "/home/dev/app/src/A.php", "/src/A.php"},
		"empty docker filesystem root":       {"/", "", "src/A.php", "/src/A.php"},
		"nested local root wins":             {"/var/www/app", "/var/www", "/var/www/app/src/A.php", "/var/www/src/A.php"},
		"nested local root container":        {"/var/www/app", "/var/www", "/var/www/src/A.php", "/var/www/src/A.php"},
		"nested docker root wins":            {"/var/www", "/var/www/app", "/var/www/app/src/A.php", "/var/www/app/src/A.php"},
		"nested docker root host":            {"/var/www", "/var/www/app", "/var/www/src/A.php", "/var/www/app/src/A.php"},
		"nested local normalized path":       {"/var/www/app", "/var/www", "/var//www/./app/src/A.php", "/var/www/src/A.php"},
		"nested docker normalized path":      {"/var/www", "/var/www/app", "/var//www/./app/src/A.php", "/var//www/./app/src/A.php"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSession(tc.local, tc.docker)
			if got := s.toContainer(tc.in); got != tc.want {
				t.Fatalf("toContainer(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToHost(t *testing.T) {
	for name, tc := range map[string]struct{ local, docker, in, want string }{
		"fileuri":                              {"/home/dev/app", "/var/www/app", "file:///var/www/app/src/A.php", "/home/dev/app/src/A.php"},
		"plain container path":                 {"/home/dev/app", "/var/www/app", "/var/www/app/src/A.php", "/home/dev/app/src/A.php"},
		"exact docker root":                    {"/home/dev/app", "/var/www/app", "file:///var/www/app", "/home/dev/app"},
		"sibling of docker root":               {"/home/dev/app", "/var/www/app", "file:///var/www/application/x.php", "/var/www/application/x.php"},
		"engine dot before root":               {"/home/dev/app", "/var/www/app", "file:///var/www/./app/src/A.php", "/home/dev/app/src/A.php"},
		"engine parent before root":            {"/home/dev/app", "/var/www/app", "file:///var/www/work/../app/src/A.php", "/home/dev/app/src/A.php"},
		"engine parent inside root":            {"/home/dev/app", "/var/www/app", "file:///var/www/app/src/../A.php", "/home/dev/app/A.php"},
		"engine parent leaves root":            {"/home/dev/app", "/var/www/app", "file:///var/www/app/../application/x.php", "/var/www/app/../application/x.php"},
		"engine repeated separators":           {"/home/dev/app", "/var/www/app", "file:///var//www/app/src//A.php", "/home/dev/app/src/A.php"},
		"roots and engine repeated separators": {"/home/dev//app", "/var//www/app", "file:///var//www/app/src/A.php", "/home/dev/app/src/A.php"},
		"normalized exact docker root":         {"/home/dev/app", "/var/www/app", "file:///var/www/./app/", "/home/dev/app"},
		"plain engine repeated separators":     {"/home/dev/app", "/var/www/app", "/var//www/app/src/A.php", "/home/dev/app/src/A.php"},
		"outside docker root":                  {"/home/dev/app", "/var/www/app", "file:///other/abs.php", "/other/abs.php"},
		"empty":                                {"/home/dev/app", "/var/www/app", "", ""},
		"empty docker root outside":            {"/home/dev/app", "", "file:///usr/share/php/lib.php", "/usr/share/php/lib.php"},
		"empty docker root local":              {"/home/dev/app", "", "file:///home/dev/app/src/A.php", "/home/dev/app/src/A.php"},
		"empty docker root verbatim":           {"/home/dev/app", "", "file:///home/dev/app/src/../A.php", "/home/dev/app/src/../A.php"},
		"empty docker root unclean outside":    {"/home/dev/app", "", "file:///usr//share/php/../lib.php", "/usr//share/php/../lib.php"},
		"empty docker root empty":              {"/home/dev/app", "", "", ""},
		"local filesystem root":                {"/", "/var/www/app", "file:///var/www/app/src/A.php", "/src/A.php"},
		"docker filesystem root":               {"/home/dev/app", "/", "file:///src/A.php", "/home/dev/app/src/A.php"},
		"exact docker filesystem root":         {"/home/dev/app", "/", "file:///", "/home/dev/app"},
		"empty docker filesystem root":         {"/", "", "file:///usr/share/php/lib.php", "/usr/share/php/lib.php"},
		"nested local root":                    {"/var/www/app", "/var/www", "file:///var/www/src/A.php", "/var/www/app/src/A.php"},
		"nested docker root":                   {"/var/www", "/var/www/app", "file:///var/www/app/src/A.php", "/var/www/src/A.php"},
		"nested local normalized path":         {"/var/www/app", "/var/www", "file:///var//www/./src/A.php", "/var/www/app/src/A.php"},
		"nested docker normalized path":        {"/var/www", "/var/www/app", "file:///var//www/./app/src/A.php", "/var/www/src/A.php"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSession(tc.local, tc.docker)
			if got := s.toHost(tc.in); got != tc.want {
				t.Fatalf("toHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToHostDecodesURI(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"spaces", "file:///d/my%20dir/x.php", "/l/my dir/x.php"},
		{"reserved characters", "file:///d/100%25%23%3F%22%5C.php", "/l/100%#?\"\\.php"},
		{"non-ASCII", "file:///d/%C5%BC%C3%B3%C5%82%C4%87.php", "/l/żółć.php"},
		{"encoded percent", "file:///d/literal%2520.php", "/l/literal%20.php"},
		{"plain percent", "/d/100%.php", "/l/100%.php"},
		{"plain percent escape", "/d/literal%20.php", "/l/literal%20.php"},
		{"plain URI characters", "/d/a#b?c.php", "/l/a#b?c.php"},
		{"malformed escape", "file:///d/100%.php", "/l/100%.php"},
		{"malformed URI stays encoded", "file:///d/my%20dir/100%.php", "/l/my%20dir/100%.php"},
		{"outside root", "file:///other/my%20dir/x.php", "/other/my dir/x.php"},
		{"non-file URI", "https://example.com/a%20b.php", "https://example.com/a%20b.php"},
		{"no file authority delimiter", "file:/d/my%20dir/x.php", "file:/d/my%20dir/x.php"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession("/l", "/d")
			if got := s.toHost(tc.in); got != tc.want {
				t.Fatalf("toHost(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBreakpointListPreservesPlainPercentEscapes(t *testing.T) {
	s := newSession("/l", "/d")
	if _, err := s.SetBreakpoint("literal%20.php", 3); err != nil {
		t.Fatal(err)
	}

	text, err := s.BreakpointList()
	want := "queued q1 /l/literal%20.php:3\n"
	if err != nil || text != want {
		t.Fatalf("BreakpointList = %q, %v; want %q", text, err, want)
	}
}

func TestLocationAndStackDecodeFileURI(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(
		`<response command="run" status="break" reason="ok"><message filename="file:///d/my%20dir/a%20b.php" lineno="3"/></response>`,
		`<response command="stack_get"><stack level="0" where="main" filename="file:///d/my%20dir/a%20b.php" lineno="3"/></response>`,
	)

	text, err := s.step("run")
	if err != nil || text != "state=break reason=ok\nlocation=/l/my dir/a b.php:3" {
		t.Fatalf("run = %q, %v; want the decoded host path", text, err)
	}
	if status := s.Status(); !strings.Contains(status, "location=/l/my dir/a b.php:3") {
		t.Fatalf("Status = %q; want the decoded host path", status)
	}
	text, err = s.Stack()
	if err != nil || text != "#0 main  /l/my dir/a b.php:3\n" {
		t.Fatalf("Stack = %q, %v; want the decoded host path", text, err)
	}
}

func TestAdoptBreakpointWithEmptyDockerRoot(t *testing.T) {
	eng, conn := newPipe(t)
	s := newSession("/home/dev/app", "")
	if _, err := s.SetBreakpoint("src/Foo.php", 10); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	command := make(chan string, 1)
	go func() {
		eng.send(xmlProlog + `<init fileuri="file:///home/dev/app/index.php"/>`)
		r := bufio.NewReader(eng.conn)
		for i := 0; i < 5; i++ {
			line, err := r.ReadString(0)
			if err != nil {
				t.Errorf("engine read: %v", err)
				return
			}
			if strings.HasPrefix(line, "breakpoint_set ") {
				command <- line
			}
			eng.send(xmlProlog + withTx(`<response command="x" id="7"/>`, txOf(line)))
		}
	}()

	s.adopt(conn)

	select {
	case line := <-command:
		if line != "breakpoint_set -i 5 -t line -f \"file:///home/dev/app/src/Foo.php\" -n 10\x00" {
			t.Fatalf("breakpoint command = %q, want a quoted absolute file URI", line)
		}
	default:
		t.Fatal("the engine did not receive breakpoint_set")
	}
}

func TestStepStoppingClearsLocation(t *testing.T) {
	s, eng := newActivePipe(t)
	s.file, s.line = "/l/t.php", 5
	go eng.respondTo("run -i 1\x00", "<response command=\"run\" status=\"stopping\" reason=\"ok\"/>")

	out, err := s.step("run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "location=-") || !strings.Contains(out, "(script finished)") {
		t.Fatalf("step output = %q, want a cleared location and script-finished marker", out)
	}
}

func TestStepStartingClearsLocation(t *testing.T) {
	s, eng := newActivePipe(t)
	s.file, s.line = "/l/t.php", 5
	go eng.respondTo("run -i 1\x00", "<response command=\"run\" status=\"starting\" reason=\"ok\"/>")

	out, err := s.step("run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "location=-") {
		t.Fatalf("step output = %q, want a cleared location", out)
	}
}

func TestStepBreakUpdatesLocation(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respondTo("step_over -i 1\x00", "<response command=\"step_over\" status=\"break\" reason=\"ok\"><message filename=\"file:///d/t.php\" lineno=\"7\"/></response>")

	out, err := s.step("step_over")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "location=/l/t.php:7") {
		t.Fatalf("step output = %q, want the new break location", out)
	}
}

func TestStopClearsLocation(t *testing.T) {
	s, eng := newActivePipe(t)
	s.file, s.line = "/l/t.php", 5
	go eng.respondTo("stop -i 1\x00", "<response command=\"stop\" status=\"stopped\"/>")

	got, err := s.Stop()
	if err != nil || got != "stopped" {
		t.Fatalf("Stop() = %q, %v", got, err)
	}
	if !strings.Contains(s.Status(), "state=no session\nlocation=-") {
		t.Fatalf("Status() = %q, want no session and no location", s.Status())
	}
}

func TestDetachClearsLocation(t *testing.T) {
	s, eng := newActivePipe(t)
	s.file, s.line = "/l/t.php", 5
	go eng.respondTo("detach -i 1\x00", "<response command=\"detach\" status=\"stopping\"/>")

	got, err := s.Detach()
	if err != nil || got != "detached" {
		t.Fatalf("Detach() = %q, %v", got, err)
	}
	if !strings.Contains(s.Status(), "state=no session\nlocation=-") {
		t.Fatalf("Status() = %q, want no session and no location", s.Status())
	}
}

func TestRawLockedWriteErrorClearsLocation(t *testing.T) {
	s, eng := newActivePipe(t)
	s.file, s.line = "/l/t.php", 5
	_ = eng.conn.Close()

	_, _, err := s.cmd("run", "")
	if err == nil {
		t.Fatal("command write error = nil, want a closed-connection error")
	}
	if s.state != "no session" || s.conn != nil || s.location() != "-" {
		t.Fatalf("state = %q, conn nil = %v, location = %q", s.state, s.conn == nil, s.location())
	}
}

func TestRawLockedReadErrorClearsLocation(t *testing.T) {
	s, eng := newActivePipe(t)
	s.file, s.line = "/l/t.php", 5
	go func() {
		_, _ = bufio.NewReader(eng.conn).ReadString(0)
		_ = eng.conn.Close()
	}()

	_, _, err := s.cmd("run", "")
	if err == nil {
		t.Fatal("command read error = nil, want an EOF")
	}
	if s.state != "no session" || s.conn != nil || s.location() != "-" {
		t.Fatalf("state = %q, conn nil = %v, location = %q", s.state, s.conn == nil, s.location())
	}
}

func TestFailHandshakeClearsLocation(t *testing.T) {
	s, _ := newActivePipe(t)
	s.file, s.line = "/l/t.php", 5

	s.mu.Lock()
	s.failHandshakeLocked("read init", errors.New("closed"))
	s.mu.Unlock()

	if s.conn != nil || s.state != "no session" || s.location() != "-" {
		t.Fatalf("state = %q, conn nil = %v, location = %q", s.state, s.conn == nil, s.location())
	}
	if s.adoptErr == nil {
		t.Fatal("handshake error = nil, want the original failure")
	}
}

// TestPauseInterruptsRun drives the case from the issue: run blocks (the engine
// does not answer it) while pause must still be able to interrupt. The engine
// answers the break first and then the pending run, both with status=break.
func TestPauseInterruptsRun(t *testing.T) {
	s, eng := newActivePipe(t)
	go func() {
		var runTx, breakTx string
		for runTx == "" || breakTx == "" {
			cmd, tx := eng.readCmd()
			switch cmd {
			case "run":
				runTx = tx
			case "break":
				breakTx = tx
			}
		}
		eng.send(xmlProlog + withTx(`<response command="break" status="break" reason="ok"/>`, breakTx))
		eng.send(xmlProlog + withTx(`<response command="run" status="break" reason="ok"><message filename="file:///d/a.php" lineno="7"/></response>`, runTx))
	}()

	runErr := make(chan error, 1)
	go func() {
		_, err := s.step("run")
		runErr <- err
	}()

	breakOut := make(chan string, 1)
	breakErr := make(chan error, 1)
	go func() {
		out, err := s.step("break")
		breakOut <- out
		breakErr <- err
	}()

	select {
	case err := <-breakErr:
		if err != nil {
			t.Fatalf("pause = %v; want it to interrupt the pending run", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pause did not return within 1s while a run was pending")
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run = %v; want it to return after the pause", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not return within 1s after the pause")
	}
	if out := <-breakOut; !strings.Contains(out, "state=break") {
		t.Fatalf("pause output = %q; want state=break", out)
	}
	if got := s.Status(); !strings.Contains(got, "state=break") {
		t.Fatalf("Status = %q; want state=break after the pause", got)
	}
}

// TestPendingCommandsFailOnDisconnect checks that a command blocked on the
// engine returns when the connection drops, and that the session is cleared.
func TestPendingCommandsFailOnDisconnect(t *testing.T) {
	s, eng := newActivePipe(t)
	runErr := make(chan error, 1)
	go func() {
		_, err := s.step("run")
		runErr <- err
	}()
	eng.readCmd() // wait until run is on the wire
	_ = eng.conn.Close()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("run = nil; want an error after the engine disconnects")
		}
	case <-time.After(time.Second):
		t.Fatal("run did not return within 1s after the disconnect")
	}
	if got := s.Status(); !strings.Contains(got, "state=no session") {
		t.Fatalf("Status = %q; want no session after the disconnect", got)
	}
}

// TestDetachWhileRunPending checks that Detach does not hang behind a pending
// run and that the run is released when the connection is dropped.
func TestDetachWhileRunPending(t *testing.T) {
	s, eng := newActivePipe(t)
	runErr := make(chan error, 1)
	go func() {
		_, err := s.step("run")
		runErr <- err
	}()
	eng.readCmd() // the run command is pending in the engine

	detached := make(chan struct{})
	go func() {
		_, _ = s.Detach()
		close(detached)
	}()
	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("Detach did not return within 1s while a run was pending")
	}
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("run = nil; want an error after detach")
		}
	case <-time.After(time.Second):
		t.Fatal("run did not return within 1s after detach")
	}
}

// TestStopWhileRunPending is the Stop counterpart of TestDetachWhileRunPending.
func TestStopWhileRunPending(t *testing.T) {
	s, eng := newActivePipe(t)
	runErr := make(chan error, 1)
	go func() {
		_, err := s.step("run")
		runErr <- err
	}()
	eng.readCmd() // the run command is pending in the engine

	stopped := make(chan struct{})
	go func() {
		_, _ = s.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return within 1s while a run was pending")
	}
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("run = nil; want an error after stop")
		}
	case <-time.After(time.Second):
		t.Fatal("run did not return within 1s after stop")
	}
}

// TestStartReaderFailsPreviousWaiters reproduces the ordering in which a new
// reader replaces the waiter table before the old reader's finishReader runs:
// the old reader then early-returns on the readerConn mismatch, so the old
// command must be failed by startReader itself instead of hanging forever.
func TestStartReaderFailsPreviousWaiters(t *testing.T) {
	s, eng := newActivePipe(t)
	runErr := make(chan error, 1)
	go func() {
		_, err := s.step("run")
		runErr <- err
	}()
	eng.readCmd() // run is pending on the old connection; its reader stays alive

	// A new connection's reader replaces the old one. The old connection is not
	// closed, so its reader cannot fail the waiter through finishReader.
	_, conn2 := newPipe(t)
	_ = conn2.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn2, bufio.NewReader(conn2))

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("the pending run must fail when a new reader replaces the old one")
		}
	case <-time.After(time.Second):
		t.Fatal("the pending run was orphaned by the new reader")
	}
}

// TestLateReaderDoesNotDisturbNewSession checks the readerConn identity guard:
// a reader for an old connection that finishes after a new one started must not
// drop the new session or its waiters.
func TestLateReaderDoesNotDisturbNewSession(t *testing.T) {
	s, _ := newActivePipe(t)
	oldConn := s.conn

	_, conn2 := newPipe(t)
	_ = conn2.SetDeadline(time.Now().Add(2 * time.Second))
	s.mu.Lock()
	s.conn = conn2
	s.r = bufio.NewReader(conn2)
	s.mu.Unlock()
	s.startReader(conn2, s.r)

	// The old reader observes its connection ending after the new reader started.
	s.finishReader(oldConn, errors.New("old connection closed"))

	if got := s.Status(); !strings.Contains(got, "state=started") {
		t.Fatalf("Status = %q; a late reader must not drop the new session", got)
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != conn2 {
		t.Fatal("a late reader must not clear the new connection")
	}
}

// TestPauseRejectedWhenAsyncBreakUnsupported checks the clear error when the
// engine reported supports_async=0 during the handshake.
func TestPauseRejectedWhenAsyncBreakUnsupported(t *testing.T) {
	s := newSession("/l", "/d")
	s.asyncBreak = false
	if _, err := s.step("break"); err == nil || !strings.Contains(err.Error(), "supports_async=0") {
		t.Fatalf("pause = %v; want a supports_async=0 error", err)
	}
}

// TestAdoptNegotiatesAsyncBreak checks that adopt reads supports_async and
// disables pause when the engine answers 0.
func TestAdoptNegotiatesAsyncBreak(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{"supported", "1", true},
		{"unsupported", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, conn := newPipe(t)
			s := newSession("/l", "/d")
			if _, err := s.SetBreakpoint("a.php", 3); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			go func() {
				eng.send(xmlProlog + `<init fileuri="file:///d/index.php"/>`)
				eng.respond(
					`<response command="feature_set" success="1"/>`,
					`<response command="feature_set" success="1"/>`,
					`<response command="feature_set" success="1"/>`,
					`<response command="feature_get"><property name="supports_async" type="int" encoding="none">`+tc.value+`</property></response>`,
					`<response command="breakpoint_set" id="7"/>`,
				)
			}()

			s.adopt(conn)
			if s.asyncBreak != tc.want {
				t.Fatalf("asyncBreak = %v; want %v for supports_async=%s", s.asyncBreak, tc.want, tc.value)
			}
		})
	}
}
