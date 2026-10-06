package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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
	eng.drain()
	go func() {
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
		for i := 0; i < 4; i++ {
			line, err := r.ReadString(0)
			if err != nil {
				t.Errorf("engine read: %v", err)
				return
			}
			if strings.HasPrefix(line, "breakpoint_set ") {
				command <- line
			}
			eng.send(xmlProlog + `<response command="x" id="7"/>`)
		}
	}()

	s.adopt(conn)

	select {
	case line := <-command:
		if !strings.Contains(line, "-f file:///home/dev/app/src/Foo.php -n 10") {
			t.Fatalf("breakpoint command = %q, want an absolute file URI", line)
		}
	default:
		t.Fatal("the engine did not receive breakpoint_set")
	}
}
