package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type bp struct {
	file string // container path
	line int
	id   string // assigned by the engine on apply
}

type session struct {
	mu      sync.Mutex
	conn    net.Conn
	r       *bufio.Reader
	tx      int
	state   string // "no session" | "started" | "break" | "stopping"
	file    string // current location, host path
	line    int
	pending []bp
	ready   chan struct{} // closed on each adopt; lets ListenWait/DoRequest await a connection

	dbgAddr string       // "host:port" where Xdebug connects (e.g. "0.0.0.0:9003")
	ln      net.Listener // non-nil only while the ephemeral listener is open

	localRoot  string
	dockerRoot string

	enableCmd  string // shell command to enable Xdebug in the container
	disableCmd string
	statusCmd  string
	projectDir string // working directory for the above commands

	containerExec string // prefix for running commands in the container, e.g. "docker compose exec -T php-sub-api"
}

func newSession(localRoot, dockerRoot string) *session {
	return &session{
		state:      "no session",
		ready:      make(chan struct{}),
		localRoot:  strings.TrimRight(localRoot, "/"),
		dockerRoot: strings.TrimRight(dockerRoot, "/"),
	}
}

// openOnce opens the DBGp port, accepts exactly one Xdebug connection, calls
// adopt(), then closes the port. The port is closed whether the session ends
// cleanly or times out, so browser/curl requests can never accidentally connect
// to a debug session that is no longer active.
//
// If the port is already in use, acquireListener waits up to portWait for it to
// become free. This lets multiple MCP instances coexist — one debugs while the
// other waits for its turn.
func (s *session) openOnce(timeout, portWait time.Duration) error {
	ln, err := s.acquireListener(portWait)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	log.Printf("DBGp listener open %s (local=%s docker=%s)", s.dbgAddr, s.localRoot, s.dockerRoot)

	go func() {
		defer func() {
			ln.Close()
			s.mu.Lock()
			if s.ln == ln {
				s.ln = nil
			}
			s.mu.Unlock()
			log.Printf("DBGp listener closed")
		}()
		ln.(*net.TCPListener).SetDeadline(time.Now().Add(timeout))
		conn, err := ln.Accept()
		if err != nil {
			return // timeout or closeLn() called
		}
		s.adopt(conn)
	}()
	return nil
}

// acquireListener tries to open the DBGp port. If our own listener is already
// open, it tells the caller to finish/stop the existing session. If another
// process holds the port, it polls every 200ms for up to portWait, then reports
// who is holding it (via lsof when available).
func (s *session) acquireListener(portWait time.Duration) (net.Listener, error) {
	s.mu.Lock()
	if s.ln != nil || s.conn != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("debug session already active — call docker_xdebug_detach or docker_xdebug_stop first")
	}
	s.mu.Unlock()

	deadline := time.Now().Add(portWait)
	for {
		// On macOS Go sets SO_REUSEADDR, so net.Listen succeeds even when
		// another process is already listening on the same port — we'd open a
		// "ghost" listener that never receives connections. Probe with lsof
		// first so we detect the conflict and wait for the port to actually be
		// free.
		if holder := portHolder(s.dbgAddr); holder != "" {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("xdebug port %s is busy (held by: %s) — another debugger is using it; wait for it to finish or stop that session", s.dbgAddr, holder)
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		ln, err := net.Listen("tcp", s.dbgAddr)
		if err == nil {
			return ln, nil
		}
		if !isAddrInUse(err) {
			return nil, fmt.Errorf("listen %s: %w", s.dbgAddr, err)
		}
		if time.Now().After(deadline) {
			holder := portHolder(s.dbgAddr)
			if holder != "" {
				return nil, fmt.Errorf("xdebug port %s is busy (held by: %s) — another debugger is using it; wait for it to finish or stop that session", s.dbgAddr, holder)
			}
			return nil, fmt.Errorf("xdebug port %s is busy — another debugger may be using it; wait for it to finish or stop that session", s.dbgAddr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isAddrInUse reports whether err is an "address already in use" listen error.
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	return strings.Contains(err.Error(), "address already in use")
}

// portHolder uses lsof to identify the process listening on addr (host:port).
// Returns a human-readable string (COMMAND PID USER) or "" if unavailable.
func portHolder(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").CombinedOutput()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			return fmt.Sprintf("%s (pid=%s user=%s)", fields[0], fields[1], fields[2])
		}
	}
	return ""
}

// closeLn closes the active listener immediately (e.g. on caller timeout).
func (s *session) closeLn() {
	s.mu.Lock()
	if s.ln != nil {
		s.ln.Close()
		s.ln = nil
	}
	s.mu.Unlock()
}

// adopt takes over a freshly accepted engine connection: reads <init>, sets
// features, applies pending breakpoints, and wakes any ListenWait/DoRequest.
func (s *session) adopt(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
	}
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.tx = 0
	s.file, s.line = "", 0

	initXML, err := s.readPacket()
	if err != nil {
		s.state = "no session"
		log.Printf("read init: %v", err)
		return
	}
	var ir struct {
		Fileuri string `xml:"fileuri,attr"`
	}
	unmarshal(initXML, &ir)
	s.state = "started"
	log.Printf("session started: %s", s.toHost(ir.Fileuri))

	s.rawLocked("feature_set", "-n max_depth -v 3")
	s.rawLocked("feature_set", "-n max_children -v 100")
	s.rawLocked("feature_set", "-n max_data -v 4096")
	for i := range s.pending {
		if r, _, err := s.rawLocked("breakpoint_set", fmt.Sprintf("-t line -f %s -n %d", fileURI(s.pending[i].file), s.pending[i].line)); err == nil && r != nil {
			s.pending[i].id = r.ID
		}
	}

	// No breakpoints: run the script to completion and finalize the session so
	// the request isn't left blocked. After `run` the engine reaches "stopping"
	// (script done) and waits for one more command before it tears down and
	// releases the connection — send `stop` to let the request return. This also
	// unblocks external requests (browser, curl) that no one drives explicitly.
	if len(s.pending) == 0 {
		r, _, _ := s.rawLocked("run", "")
		if r != nil && r.Status == "stopping" {
			s.rawLocked("stop", "")
		}
		if s.conn != nil {
			s.conn.Close()
			s.conn = nil
		}
		s.state = "no session"
	}

	close(s.ready)
	s.ready = make(chan struct{})
}

// --- wire protocol ----------------------------------------------------------

// readPacket reads one length-prefixed, NUL-terminated DBGp packet: LEN\0XML\0
func (s *session) readPacket() (string, error) {
	lenStr, err := s.r.ReadString(0)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimRight(lenStr, "\x00"))
	if err != nil {
		return "", fmt.Errorf("bad length %q: %w", lenStr, err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return "", err
	}
	s.r.ReadByte() // trailing NUL
	return string(buf), nil
}

// rawLocked sends one command and returns the parsed response. Caller holds mu.
func (s *session) rawLocked(name, args string) (*xResp, string, error) {
	if s.conn == nil {
		return nil, "", fmt.Errorf("no active session")
	}
	s.tx++
	line := name + " -i " + strconv.Itoa(s.tx)
	if args != "" {
		line += " " + args
	}
	if _, err := s.conn.Write([]byte(line + "\x00")); err != nil {
		s.state = "no session"
		return nil, "", err
	}
	xmlStr, err := s.readPacket()
	if err != nil {
		s.state = "no session"
		return nil, "", err
	}
	var r xResp
	unmarshal(xmlStr, &r)
	if r.Status != "" {
		s.state = r.Status
	}
	if r.Message != nil && r.Message.Filename != "" {
		s.file, s.line = s.toHost(r.Message.Filename), r.Message.Lineno
	}
	return &r, xmlStr, nil
}

// cmd is the locking wrapper used by public methods.
func (s *session) cmd(name, args string) (*xResp, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rawLocked(name, args)
}

// --- path translation -------------------------------------------------------

// toContainer maps a host (absolute or project-relative) path to the container path.
func (s *session) toContainer(p string) string {
	switch {
	case strings.HasPrefix(p, s.dockerRoot):
		return p
	case strings.HasPrefix(p, s.localRoot):
		return s.dockerRoot + p[len(s.localRoot):]
	case strings.HasPrefix(p, "/"):
		return p // some other absolute path; pass through
	default:
		return s.dockerRoot + "/" + strings.TrimLeft(p, "/")
	}
}

// toHost maps a container fileuri/path back to a host path for display.
func (s *session) toHost(fileuri string) string {
	p := strings.TrimPrefix(fileuri, "file://")
	if strings.HasPrefix(p, s.dockerRoot) {
		return s.localRoot + p[len(s.dockerRoot):]
	}
	return p
}

func fileURI(containerPath string) string { return "file://" + containerPath }

// --- public command methods (used by both MCP and HTTP front-ends) ----------

func (s *session) location() string {
	if s.file == "" {
		return "-"
	}
	return fmt.Sprintf("%s:%d", s.file, s.line)
}

func (s *session) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("state=%s\nlocation=%s\nbreakpoints=%d", s.state, s.location(), len(s.pending))
}

func (s *session) SetBreakpoint(file string, line int) (string, error) {
	if file == "" || line <= 0 {
		return "", fmt.Errorf("file and line>0 required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cpath := s.toContainer(file)
	b := bp{file: cpath, line: line}
	if s.conn != nil && (s.state == "started" || s.state == "break") {
		r, _, err := s.rawLocked("breakpoint_set", fmt.Sprintf("-t line -f %s -n %d", fileURI(cpath), line))
		if err != nil {
			return "", err
		}
		b.id = r.ID
		s.pending = append(s.pending, b)
		return fmt.Sprintf("breakpoint set id=%s %s:%d", b.id, cpath, line), nil
	}
	s.pending = append(s.pending, b)
	return fmt.Sprintf("breakpoint queued %s:%d (applied on next session)", cpath, line), nil
}

func (s *session) BreakpointList() (string, error) {
	r, _, err := s.cmd("breakpoint_list", "")
	if err != nil {
		return "", err
	}
	if len(r.Breakpoints) == 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		var b strings.Builder
		for _, p := range s.pending {
			fmt.Fprintf(&b, "queued %s:%d\n", s.toHost(p.file), p.line)
		}
		if b.Len() == 0 {
			return "(none)", nil
		}
		return b.String(), nil
	}
	var b strings.Builder
	for _, e := range r.Breakpoints {
		fmt.Fprintf(&b, "id=%s %s %s:%d\n", e.ID, e.State, s.toHost(e.Filename), e.Lineno)
	}
	return b.String(), nil
}

func (s *session) BreakpointRemove(id string) (string, error) {
	s.mu.Lock()
	for i, p := range s.pending {
		if p.id == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if _, _, err := s.cmd("breakpoint_remove", "-d "+id); err != nil {
		return "", err
	}
	return "removed " + id, nil
}

// BreakpointClearAll removes every breakpoint: queued (not yet applied) and
// applied (active in the engine). Safe to call with or without an active session.
func (s *session) BreakpointClearAll() (string, error) {
	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	// If a session is active, tell the engine to drop each applied breakpoint.
	for _, p := range pending {
		if p.id != "" {
			s.cmd("breakpoint_remove", "-d "+p.id)
		}
	}
	return fmt.Sprintf("cleared %d breakpoint(s)", len(pending)), nil
}

// step runs run/step_into/step_over/step_out/break and reports the new location.
func (s *session) step(cmd string) (string, error) {
	r, _, err := s.cmd(cmd, "")
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := fmt.Sprintf("state=%s reason=%s\nlocation=%s", r.Status, r.Reason, s.location())
	if r.Status == "stopping" {
		out += "\n(script finished)"
	}
	return out, nil
}

func (s *session) Stack() (string, error) {
	r, _, err := s.cmd("stack_get", "")
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, st := range r.Stacks {
		fmt.Fprintf(&b, "#%d %s  %s:%d\n", st.Level, st.Where, s.toHost(st.Filename), st.Lineno)
	}
	if b.Len() == 0 {
		return "(no stack — not paused?)", nil
	}
	return b.String(), nil
}

func (s *session) Context(depth int) (string, error) {
	r, _, err := s.cmd("context_get", "-d "+strconv.Itoa(depth))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range r.Props {
		fmt.Fprintf(&b, "%s (%s) = %s\n", p.Name, p.Type, summarize(p))
	}
	if b.Len() == 0 {
		return "(no variables)", nil
	}
	return b.String(), nil
}

func (s *session) Eval(expr string) (string, error) {
	enc := base64.StdEncoding.EncodeToString([]byte(expr))
	r, _, err := s.cmd("eval", "-- "+enc)
	if err != nil {
		return "", err
	}
	if r.Error != nil {
		return "", fmt.Errorf("eval error %s: %s", r.Error.Code, r.Error.Message)
	}
	if len(r.Props) == 0 {
		return "(no result)", nil
	}
	return summarize(r.Props[0]), nil
}

func (s *session) PropertyGet(name string, depth int) (string, error) {
	r, _, err := s.cmd("property_get", fmt.Sprintf("-d %d -n %s", depth, name))
	if err != nil {
		return "", err
	}
	if len(r.Props) == 0 {
		return "(not found)", nil
	}
	return summarize(r.Props[0]), nil
}

func (s *session) PropertySet(name, value string) (string, error) {
	enc := base64.StdEncoding.EncodeToString([]byte(value))
	if _, _, err := s.cmd("property_set", fmt.Sprintf("-n %s -- %s", name, enc)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s = %s", name, value), nil
}

func (s *session) Detach() (string, error) {
	s.cmd("detach", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	s.state = "no session"
	return "detached", nil
}

func (s *session) Stop() (string, error) {
	s.cmd("stop", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	s.state = "no session"
	return "stopped", nil
}

func (s *session) Raw(cmd string) (string, error) {
	parts := strings.SplitN(cmd, " ", 2)
	args := ""
	if len(parts) == 2 {
		args = parts[1]
	}
	_, xmlStr, err := s.cmd(parts[0], args)
	return xmlStr, err
}

// XdebugEnable/Disable/ContainerStatus run the user-supplied shell commands.

func (s *session) XdebugEnable() (string, error)          { return s.runShell(s.enableCmd) }
func (s *session) XdebugDisable() (string, error)         { return s.runShell(s.disableCmd) }
func (s *session) XdebugContainerStatus() (string, error) { return s.runShell(s.statusCmd) }

func (s *session) runShell(cmd string) (string, error) {
	if cmd == "" {
		return "", fmt.Errorf("command not configured (pass the relevant --xdebug-*-cmd flag)")
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = s.projectDir
	out, err := c.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("%s: %w", text, err)
	}
	return text, nil
}

// ListenWait opens the DBGp port, blocks until the next engine connection is
// adopted, then closes the port. Use for CLI/Symfony commands launched separately.
func (s *session) ListenWait(timeout time.Duration) (string, error) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()

	if err := s.openOnce(timeout, 10*time.Second); err != nil {
		return "", err
	}

	select {
	case <-ready:
		return s.Status(), nil
	case <-time.After(timeout):
		s.closeLn()
		return "", fmt.Errorf("no engine connected within %s", timeout)
	}
}

// ListenFireForget opens the DBGp port and returns immediately — the listener
// stays open and the next Xdebug connection will be adopted. Use when the
// caller wants to arm the listener and then trigger the command separately
// (e.g. via run_command) without blocking on listen. Check docker_xdebug_status
// later to see if a session was adopted.
func (s *session) ListenFireForget() (string, error) {
	// We don't know the accept timeout here — use a long default (1h) so the
	// listener stays open. It'll be closed when adopt() runs.
	if err := s.openOnce(time.Hour, 10*time.Second); err != nil {
		return "", err
	}
	return "listener armed (fire-and-forget); check docker_xdebug_status to see if a session was adopted", nil
}

// RunCommand executes a command inside the container (e.g. a Symfony console
// command) and waits for the resulting Xdebug connection. Like doAndWait but
// for CLI commands instead of HTTP requests. When no breakpoints are set, the
// script runs to completion and the command output is returned. When
// breakpoints are set, the session pauses and the caller drives it with
// run/step — the command output is not available until the script finishes.
func (s *session) RunCommand(command string, timeout time.Duration) (string, error) {
	if command == "" {
		return "", fmt.Errorf("command required")
	}
	if s.containerExec == "" {
		return "", fmt.Errorf("container-exec not configured (pass --container-exec flag)")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()

	if err := s.openOnce(timeout, 10*time.Second); err != nil {
		return "", err
	}

	type cmdResult struct {
		out string
		err error
	}
	resultCh := make(chan cmdResult, 1)

	go func() {
		fullCmd := s.containerExec + " " + command
		c := exec.Command("sh", "-c", fullCmd)
		c.Dir = s.projectDir
		out, err := c.CombinedOutput()
		resultCh <- cmdResult{strings.TrimSpace(string(out)), err}
		if err != nil {
			log.Printf("command error: %v", err)
		} else {
			log.Printf("command completed: %s", command)
		}
	}()

	select {
	case <-ready:
		s.mu.Lock()
		state := s.state
		s.mu.Unlock()
		if state == "stopping" || state == "no session" {
			// Script ran to completion — collect command output.
			select {
			case r := <-resultCh:
				if r.err != nil {
					return r.out, fmt.Errorf("%s: %w", r.out, r.err)
				}
				if r.out != "" {
					return r.out, nil
				}
				return "command completed", nil
			case <-time.After(5 * time.Second):
				return "script ran to completion (command output not captured in time)", nil
			}
		}
		return "command fired; session paused at script start — call run/step to drive", nil
	case <-time.After(timeout):
		s.closeLn()
		return "", fmt.Errorf("no Xdebug connection within %s — is Xdebug enabled in the container? (docker compose exec php-sub-api xdebug 1)", timeout)
	}
}
