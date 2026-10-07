package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxPacketLen = 64 << 20 // 64 MiB

// maxSkippedPackets limits how many non-matching packets the reader drops
// before it fails the waiting commands.
const maxSkippedPackets = 1000

// handshakeTimeout bounds the DBGp handshake (the <init> packet plus the
// feature/breakpoint round trips) after Xdebug has connected. It is a variable
// so tests can shorten it.
var handshakeTimeout = 10 * time.Second

// handshakeGrace is added to the caller-side wait for a connected engine, so
// adopt's own handshakeTimeout deadline always fires first and reports the
// failure instead of the caller racing it.
var handshakeGrace = 2 * time.Second

type bp struct {
	file string // container path
	line int
	id   string // assigned by the engine on apply
	qid  string // stable local handle
	err  string // why the last apply failed
}

// readResult is one packet (or failure) delivered by the reader goroutine to a
// waiting command.
type readResult struct {
	xml string
	err error
}

// waiter is one in-flight DBGp command: the reader delivers its reply (or a
// failure) on ch, and done is closed by the command when it stops waiting so
// the reader never blocks on a channel nobody reads.
type waiter struct {
	ch   chan readResult
	done chan struct{}
}

type session struct {
	mu         sync.Mutex
	conn       net.Conn
	r          *bufio.Reader
	tx         int
	state      string // "no session" | "started" | "break" | "stopping"
	file       string // current location, host path
	line       int
	pending    []bp
	nextQID    int           // never reset when breakpoints are removed or cleared
	ready      chan struct{} // closed on each adopt; lets ListenWait/DoRequest await a connection
	adoptErr   error         // why the last adopt failed (nil on success); read by the waiters after ready closes
	asyncBreak bool          // engine supports `break` while running (supports_async); optimistic by default

	// Concurrency: wmu serializes the actual conn.Write of every command (and
	// nothing else). Reading is owned by one reader goroutine per connection,
	// which routes each <response> to its waiter by transaction_id. rmu guards
	// the reader bookkeeping below. The command's conn and tx are read/written
	// under mu so adopt (which holds mu for the whole handshake) is race-free.
	wmu        sync.Mutex
	rmu        sync.Mutex
	readerConn net.Conn        // the connection the current reader serves; nil when no reader runs
	waiters    map[int]*waiter // transaction_id -> waiting command

	// acceptedConn is the freshly accepted engine connection between Accept and
	// adopt taking it over. A caller timeout closes it so an adopt that has not
	// started yet cannot leave an orphan session. Guarded by mu.
	acceptedConn net.Conn

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
	localRoot = path.Clean(localRoot)
	if dockerRoot != "" {
		dockerRoot = path.Clean(dockerRoot)
	}
	return &session{
		state:      "no session",
		ready:      make(chan struct{}),
		asyncBreak: true, // assume the engine can break while running until it says otherwise
		localRoot:  localRoot,
		dockerRoot: dockerRoot,
	}
}

// openOnce opens the DBGp port, accepts exactly one Xdebug connection, calls
// adopt(), then closes the port. The port is closed whether the session ends
// cleanly or times out, so browser/curl requests can never accidentally connect
// to a debug session that is no longer active.
//
// It returns acceptResult, which receives the listener's Accept error: nil once
// a connection was accepted (before the DBGp handshake starts), or a non-nil
// error when the accept deadline expired or the listener was closed. Waiters
// use it to tell "no engine connected" from "engine connected but the handshake
// is still running".
//
// If the port is already in use, acquireListener waits up to portWait for it to
// become free. This lets multiple MCP instances coexist — one debugs while the
// other waits for its turn.
func (s *session) openOnce(timeout, portWait time.Duration) (<-chan error, error) {
	ln, err := s.acquireListener(portWait)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	log.Printf("DBGp listener open %s (local=%s docker=%s)", s.dbgAddr, s.localRoot, s.dockerRoot)

	acceptResult := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("DBGp accept goroutine panic: %v", r)
				select {
				case acceptResult <- fmt.Errorf("DBGp accept goroutine panic: %v", r):
				default:
				}
			}
		}()
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
			acceptResult <- err
			return // timeout or closeLn() called
		}
		// Publish the accepted connection before waking the waiter, so a caller
		// timeout can always drop it even if adopt has not started yet.
		s.mu.Lock()
		s.acceptedConn = conn
		s.mu.Unlock()
		acceptResult <- nil
		s.adopt(conn)
	}()
	return acceptResult, nil
}

// acquireListener tries to open the DBGp port. If our own listener is already
// open, it tells the caller to finish/stop the existing session. If another
// process holds the port, it polls every 200ms for up to portWait, then reports
// who is holding it (via lsof when available).
func (s *session) acquireListener(portWait time.Duration) (net.Listener, error) {
	s.mu.Lock()
	if s.ln != nil || s.conn != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("debug session already active — call detach or stop first")
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
	s.acceptedConn = nil
	if s.conn != nil {
		s.conn.Close()
	}
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.wmu.Lock()
	s.tx = 0
	s.wmu.Unlock()
	s.file, s.line = "", 0
	s.adoptErr = nil
	// A stuck or half-connected engine must not hold s.mu forever: the deadline
	// turns it into a read/write error, which failHandshakeLocked reports.
	conn.SetDeadline(time.Now().Add(handshakeTimeout))

	initXML, err := s.readPacket()
	if err != nil {
		s.failHandshakeLocked("read init", err)
		return
	}
	var ir struct {
		Fileuri string `xml:"fileuri,attr"`
	}
	if err := unmarshal(initXML, &ir); err != nil {
		s.failHandshakeLocked("parse init", err)
		return
	}
	s.state = "started"
	log.Printf("session started: %s", s.toHost(ir.Fileuri))

	s.rawLocked("feature_set", "-n max_depth -v 3")      //nolint:errcheck // best-effort feature negotiation
	s.rawLocked("feature_set", "-n max_children -v 100") //nolint:errcheck // best-effort feature negotiation
	s.rawLocked("feature_set", "-n max_data -v 4096")    //nolint:errcheck // best-effort feature negotiation
	s.negotiateAsyncBreakLocked()
	for i := range s.pending {
		p := &s.pending[i]
		p.id, p.err = "", ""
		r, _, err := s.rawLocked("breakpoint_set", breakpointSetArgs(p.file, p.line))
		if err != nil {
			p.err = err.Error()
			log.Printf("breakpoint %s:%d rejected: %v", p.file, p.line, err)
			continue
		}
		p.id = r.ID
	}

	// No breakpoints: run the script to completion and finalize the session so
	// the request isn't left blocked. After `run` the engine reaches "stopping"
	// (script done) and waits for one more command before it tears down and
	// releases the connection — send `stop` to let the request return. This also
	// unblocks external requests (browser, curl) that no one drives explicitly.
	if len(s.pending) == 0 {
		r, _, _ := s.rawLocked("run", "")
		if r != nil && r.Status == "stopping" {
			s.rawLocked("stop", "") //nolint:errcheck // best-effort stop
		}
		s.dropLocked()
	}
	if s.conn != nil {
		s.conn.SetDeadline(time.Time{})
		s.startReader(s.conn, s.r)
	}

	s.signalReadyLocked()
}

// negotiateAsyncBreakLocked asks the engine whether it can interrupt a running
// script (DBGp feature supports_async). When the engine answers "0", pause
// cannot work and cmd("break") is rejected early instead of blocking. Any
// failure (old engine, no property, read error) leaves the optimistic default
// in place. Caller holds mu; only runs during adopt, before the reader starts.
func (s *session) negotiateAsyncBreakLocked() {
	r, _, err := s.rawLocked("feature_get", "-n supports_async")
	if err != nil || r == nil || len(r.Props) == 0 {
		return
	}
	s.asyncBreak = strings.TrimSpace(decodeVal(r.Props[0])) != "0"
	if !s.asyncBreak {
		log.Printf("engine reports supports_async=0: pause cannot interrupt a running run")
	}
}

// dropLocked closes the engine connection and resets the session state.
// s.mu must be held.
func (s *session) dropLocked() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.state = "no session"
	s.clearLocationLocked()
}

// failHandshakeLocked drops the connection after a failed DBGp handshake and
// wakes the waiters with an error. s.mu must be held.
func (s *session) failHandshakeLocked(step string, err error) {
	log.Printf("%s: %v", step, err)
	s.dropLocked()
	s.adoptErr = fmt.Errorf("DBGp handshake failed after Xdebug connected (%s): %w", step, err)
	s.signalReadyLocked()
}

// signalReadyLocked wakes everyone waiting on s.ready and arms a new channel
// for the next connection. s.mu must be held.
func (s *session) signalReadyLocked() {
	close(s.ready)
	s.ready = make(chan struct{})
}

// handshakeError returns the error of the last failed adopt, or nil.
func (s *session) handshakeError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptErr
}

// --- wire protocol ----------------------------------------------------------

// readPacket reads one length-prefixed, NUL-terminated DBGp packet: LEN\0XML\0
func (s *session) readPacket() (string, error) { return readPacketFrom(s.r) }

// readPacketFrom reads one DBGp packet from r. It is shared by the synchronous
// handshake (readPacket) and the per-connection reader goroutine.
func readPacketFrom(r *bufio.Reader) (string, error) {
	lenStr, err := r.ReadString(0)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimRight(lenStr, "\x00"))
	if err != nil {
		return "", fmt.Errorf("bad length %q: %w", lenStr, err)
	}
	if n < 0 || n > maxPacketLen {
		return "", fmt.Errorf("bad length %d (max %d)", n, maxPacketLen)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	b, err := r.ReadByte()
	if err != nil {
		return "", fmt.Errorf("read trailing NUL: %w", err)
	}
	if b != 0 {
		return "", fmt.Errorf("expected NUL after packet, got %q", b)
	}
	return string(buf), nil
}

// rawLocked sends one command and reads its reply synchronously. It is used by
// the adopt handshake (before the reader goroutine starts) and by tests; normal
// commands go through cmd, which does not hold s.mu while waiting. Caller holds mu.
func (s *session) rawLocked(name, args string) (*xResp, string, error) {
	if s.conn == nil {
		return nil, "", fmt.Errorf("no active session")
	}
	s.wmu.Lock()
	s.tx++
	tx := s.tx
	line := name + " -i " + strconv.Itoa(tx)
	if args != "" {
		line += " " + args
	}
	_, werr := s.conn.Write([]byte(line + "\x00"))
	s.wmu.Unlock()
	if werr != nil {
		s.dropLocked()
		return nil, "", werr
	}
	want := strconv.Itoa(tx)
	var r xResp
	var xmlStr string
	for skipped := 0; ; skipped++ {
		if skipped > maxSkippedPackets {
			return nil, "", fmt.Errorf("%s: no response with transaction_id %s after %d other packets", name, want, maxSkippedPackets)
		}
		var err error
		xmlStr, err = s.readPacket()
		if err != nil {
			s.dropLocked() // keep the #36 behaviour: drop the connection on a read error
			return nil, "", err
		}
		r = xResp{}
		if err = unmarshal(xmlStr, &r); err != nil {
			return nil, xmlStr, fmt.Errorf("parse %s response: %w", name, err)
		}
		if r.XMLName.Local == "response" && r.TransactionID == want {
			break
		}
		log.Printf("dropping DBGp packet while waiting for %s (tx %s): <%s transaction_id=%q>", name, want, r.XMLName.Local, r.TransactionID)
	}
	if r.Status != "" {
		s.state = r.Status
		if r.Status != "break" {
			s.clearLocationLocked()
		}
	}
	if r.Message != nil && r.Message.Filename != "" {
		s.file, s.line = s.toHost(r.Message.Filename), r.Message.Lineno
	}
	return &r, xmlStr, r.err(name)
}

// cmd sends one DBGp command and returns its response. It does not hold s.mu
// while waiting, so another MCP call (e.g. pause) can run at the same time.
func (s *session) cmd(name, args string) (*xResp, string, error) {
	return s.cmdWithin(name, args, 0)
}

// cmdWithin is cmd with an optional deadline. A non-zero timeout bounds both
// the write (via a write deadline) and the wait for the reply, which is what
// Detach/Stop use so they cannot hang behind a pending run. It returns
// "no active session" when the connection is gone, and the reader's error when
// the connection drops while waiting.
func (s *session) cmdWithin(name, args string, timeout time.Duration) (*xResp, string, error) {
	// Read conn under mu so adopt (which holds mu for the whole handshake)
	// cannot race with a command issued mid-handshake.
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return nil, "", fmt.Errorf("no active session")
	}

	w := &waiter{ch: make(chan readResult, 1), done: make(chan struct{})}
	s.wmu.Lock()
	s.rmu.Lock()
	if s.readerConn != conn {
		s.rmu.Unlock()
		s.wmu.Unlock()
		return nil, "", fmt.Errorf("no active session")
	}
	// Assign the transaction id only once we know the command will be sent, so
	// a rejected call does not leave a gap. tx is guarded by wmu here; adopt's
	// handshake writes it under mu but always finishes before a reader starts.
	s.tx++
	tx := s.tx
	s.waiters[tx] = w
	s.rmu.Unlock()

	line := name + " -i " + strconv.Itoa(tx)
	if args != "" {
		line += " " + args
	}
	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	_, werr := conn.Write([]byte(line + "\x00"))
	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	s.wmu.Unlock()
	if werr != nil {
		s.removeWaiter(tx, w)
		close(w.done)
		s.dropConn(conn)
		return nil, "", werr
	}
	defer close(w.done)
	defer s.removeWaiter(tx, w)

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timeoutCh = t.C
	}
	select {
	case res := <-w.ch:
		return s.finishRead(name, strconv.Itoa(tx), res)
	case <-timeoutCh:
		return nil, "", fmt.Errorf("%s: no response within %s", name, timeout)
	}
}

// finishRead parses a reply delivered by the reader and applies it to the
// session. It mirrors the matching rawLocked does during the handshake.
func (s *session) finishRead(name, wantTx string, res readResult) (*xResp, string, error) {
	if res.err != nil {
		if res.xml != "" {
			return nil, res.xml, fmt.Errorf("parse %s response: %w", name, res.err)
		}
		return nil, "", res.err
	}
	var r xResp
	if err := unmarshal(res.xml, &r); err != nil {
		return nil, res.xml, fmt.Errorf("parse %s response: %w", name, err)
	}
	if r.XMLName.Local != "response" || r.TransactionID != wantTx {
		return nil, res.xml, fmt.Errorf("%s: unexpected DBGp packet <%s transaction_id=%q>", name, r.XMLName.Local, r.TransactionID)
	}
	s.applyResp(&r)
	return &r, res.xml, r.err(name)
}

// applyResp records the state and location carried by a response.
func (s *session) applyResp(r *xResp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Status != "" {
		s.state = r.Status
		if r.Status != "break" {
			s.clearLocationLocked()
		}
	}
	if r.Message != nil && r.Message.Filename != "" {
		s.file, s.line = s.toHost(r.Message.Filename), r.Message.Lineno
	}
}

// removeWaiter drops tx from the waiter table when it is still ours.
func (s *session) removeWaiter(tx int, w *waiter) {
	s.rmu.Lock()
	if s.waiters != nil && s.waiters[tx] == w {
		delete(s.waiters, tx)
	}
	s.rmu.Unlock()
}

// dropConn closes the session when conn is still the active connection.
func (s *session) dropConn(conn net.Conn) {
	s.mu.Lock()
	if s.conn == conn {
		s.dropLocked()
	}
	s.mu.Unlock()
}

// --- reader goroutine -------------------------------------------------------

// startReader launches the goroutine that owns reading for conn. It is called
// at the end of a successful adopt, after the synchronous handshake is done, so
// the reader never steals the <init> or handshake replies. Each connection gets
// a fresh waiter table, so a late reader for an old connection cannot disturb
// the current one.
//
// Replacing the reader also orphans any command still waiting on the previous
// connection: its reader now sees a readerConn mismatch and returns without
// failing them. Fail them here with a connection-closed error so a command such
// as a timeout-less run cannot block forever.
func (s *session) startReader(conn net.Conn, r *bufio.Reader) {
	s.rmu.Lock()
	previous := s.waiters
	s.readerConn = conn
	s.waiters = make(map[int]*waiter)
	s.rmu.Unlock()
	for _, w := range previous {
		deliver(w, readResult{err: fmt.Errorf("connection closed")})
	}
	go s.readLoop(conn, r)
}

// readLoop reads packets and routes each <response> to its waiter by
// transaction_id. Stream/notify/unsolicited packets are logged and dropped, as
// rawLocked did (#33); a malformed packet fails every waiter because it cannot
// be attributed. When the connection ends, every pending waiter is failed and
// the session is dropped.
func (s *session) readLoop(conn net.Conn, r *bufio.Reader) {
	skipped := 0
	for {
		xmlStr, err := readPacketFrom(r)
		if err != nil {
			s.finishReader(conn, err)
			return
		}
		var resp xResp
		if perr := unmarshal(xmlStr, &resp); perr != nil {
			log.Printf("dropping malformed DBGp packet: %v", perr)
			s.failWaiters(conn, func(int) error { return perr }, xmlStr)
			continue
		}
		if resp.XMLName.Local == "response" && resp.TransactionID != "" {
			if s.deliverResponse(conn, resp.TransactionID, xmlStr) {
				skipped = 0
				continue
			}
		}
		log.Printf("dropping DBGp packet while waiting: <%s transaction_id=%q>", resp.XMLName.Local, resp.TransactionID)
		skipped++
		if skipped > maxSkippedPackets {
			s.failWaiters(conn, func(tx int) error {
				return fmt.Errorf("no response with transaction_id %d after %d other packets", tx, maxSkippedPackets)
			}, "")
			skipped = 0
		}
	}
}

// deliverResponse hands a response to the waiter for tx, if one is registered
// for the current connection. It reports whether a waiter existed.
func (s *session) deliverResponse(conn net.Conn, txStr, xmlStr string) bool {
	tx, err := strconv.Atoi(txStr)
	if err != nil {
		return false
	}
	s.rmu.Lock()
	if s.readerConn != conn {
		s.rmu.Unlock()
		return false
	}
	w := s.waiters[tx]
	s.rmu.Unlock()
	if w == nil {
		return false
	}
	deliver(w, readResult{xml: xmlStr})
	return true
}

// failWaiters fails every registered waiter with the given reason and clears
// the table. reason builds the error for each transaction id.
func (s *session) failWaiters(conn net.Conn, reason func(int) error, xmlStr string) {
	s.rmu.Lock()
	if s.readerConn != conn {
		s.rmu.Unlock()
		return
	}
	ws := s.waiters
	s.waiters = make(map[int]*waiter)
	s.rmu.Unlock()
	for tx, w := range ws {
		deliver(w, readResult{xml: xmlStr, err: reason(tx)})
	}
}

// finishReader drops the session (when conn is still active) and then fails the
// pending waiters, so a caller woken by the failure observes state=no session.
func (s *session) finishReader(conn net.Conn, err error) {
	s.rmu.Lock()
	if s.readerConn != conn {
		s.rmu.Unlock()
		return
	}
	ws := s.waiters
	s.waiters = nil
	s.readerConn = nil
	s.rmu.Unlock()

	s.mu.Lock()
	if s.conn == conn {
		s.dropLocked()
	}
	s.mu.Unlock()

	for _, w := range ws {
		deliver(w, readResult{err: err})
	}
}

// deliver sends res to w without ever blocking: if the command already stopped
// waiting, w.done is closed and the result is dropped.
func deliver(w *waiter, res readResult) {
	select {
	case w.ch <- res:
	case <-w.done:
	}
}

// --- path translation -------------------------------------------------------

// under reports whether p equals root or is inside root at a path boundary.
func under(p, root string) bool {
	if root == "/" {
		return path.IsAbs(p)
	}
	return root != "" && (p == root || strings.HasPrefix(p, root+"/"))
}

// toContainer maps a host (absolute or project-relative) path to the container path.
func (s *session) toContainer(p string) string {
	if s.dockerRoot == "" {
		if path.IsAbs(p) {
			return p
		}
		return path.Join(s.localRoot, p)
	}
	cleanPath := path.Clean(p)
	switch {
	// When both roots match, the more specific root takes precedence.
	case under(cleanPath, s.localRoot) && (!under(cleanPath, s.dockerRoot) || len(s.localRoot) > len(s.dockerRoot)):
		return path.Join(s.dockerRoot, strings.TrimPrefix(cleanPath, s.localRoot))
	case under(cleanPath, s.dockerRoot):
		return p
	case path.IsAbs(p):
		return p // some other absolute path; pass through
	default:
		return path.Join(s.dockerRoot, p)
	}
}

// toHost maps a container fileuri/path back to a host path for display.
func (s *session) toHost(fileuri string) string {
	p := fileuri
	if strings.HasPrefix(fileuri, "file://") {
		if u, err := url.Parse(fileuri); err == nil {
			p = u.Path
		} else {
			p = strings.TrimPrefix(fileuri, "file://")
		}
	}
	cleanPath := path.Clean(p)
	if under(cleanPath, s.dockerRoot) {
		return path.Join(s.localRoot, strings.TrimPrefix(cleanPath, s.dockerRoot))
	}
	return p
}

func fileURI(containerPath string) string {
	return (&url.URL{Scheme: "file", Path: containerPath}).String()
}

func breakpointSetArgs(containerPath string, line int) string {
	return fmt.Sprintf("-t line -f %s -n %d", quoteArg(fileURI(containerPath)), line)
}

// --- public command methods (used by both MCP and HTTP front-ends) ----------

func (s *session) clearLocationLocked() {
	s.file, s.line = "", 0
}

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
	cpath := s.toContainer(file)
	s.nextQID++
	b := bp{file: cpath, line: line, qid: "q" + strconv.Itoa(s.nextQID)}
	live := s.conn != nil && (s.state == "started" || s.state == "break")
	s.mu.Unlock()
	if !live {
		s.mu.Lock()
		s.pending = append(s.pending, b)
		s.mu.Unlock()
		return fmt.Sprintf("breakpoint queued %s %s:%d (applied on next session)", b.qid, cpath, line), nil
	}
	r, _, err := s.cmd("breakpoint_set", breakpointSetArgs(cpath, line))
	if err != nil {
		return "", err
	}
	b.id = r.ID
	s.mu.Lock()
	s.pending = append(s.pending, b)
	s.mu.Unlock()
	return fmt.Sprintf("breakpoint set id=%s %s:%d", b.id, cpath, line), nil
}

func (s *session) BreakpointList() (string, error) {
	s.mu.Lock()
	hasConn := s.conn != nil
	s.mu.Unlock()
	var b strings.Builder
	if hasConn {
		r, _, err := s.cmd("breakpoint_list", "")
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		for _, e := range r.Breakpoints {
			fmt.Fprintf(&b, "id=%s %s %s:%d\n", e.ID, e.State, s.toHost(e.Filename), e.Lineno)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pending {
		if p.err != "" {
			fmt.Fprintf(&b, "rejected %s %s:%d: %s\n", p.qid, s.toHost(p.file), p.line, p.err)
		} else if s.conn == nil || p.id == "" {
			fmt.Fprintf(&b, "queued %s %s:%d\n", p.qid, s.toHost(p.file), p.line)
		}
	}
	if b.Len() == 0 {
		return "(none)", nil
	}
	return b.String(), nil
}

func validateBreakpointID(id string) error {
	if id == "" {
		return fmt.Errorf("breakpoint id must be numeric")
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return fmt.Errorf("breakpoint id must be numeric: %q", id)
		}
	}
	return nil
}

func (s *session) BreakpointRemove(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("id required")
	}
	s.mu.Lock()
	index, engineID := -1, id
	for i, p := range s.pending {
		if p.qid == id || p.id == id {
			index, engineID = i, p.id
			break
		}
	}
	if engineID != "" {
		if err := validateBreakpointID(engineID); err != nil {
			s.mu.Unlock()
			return "", err
		}
	}
	hasConn := s.conn != nil
	s.mu.Unlock()
	if index == -1 && !hasConn {
		return "", fmt.Errorf("no active session")
	}
	if hasConn && engineID != "" {
		if _, _, err := s.cmd("breakpoint_remove", "-d "+engineID); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if index != -1 {
		s.pending = append(s.pending[:index], s.pending[index+1:]...)
	}
	return "removed " + id, nil
}

// BreakpointClearAll removes every breakpoint: queued (not yet applied) and
// applied (active in the engine). Safe to call with or without an active session.
func (s *session) BreakpointClearAll() (string, error) {
	s.mu.Lock()
	for _, p := range s.pending {
		if p.id != "" {
			if err := validateBreakpointID(p.id); err != nil {
				s.mu.Unlock()
				return "", err
			}
		}
	}
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	// If a session is active, tell the engine to drop each applied breakpoint.
	for _, p := range pending {
		if p.id != "" {
			s.cmd("breakpoint_remove", "-d "+p.id) //nolint:errcheck // best-effort cleanup
		}
	}
	return fmt.Sprintf("cleared %d breakpoint(s)", len(pending)), nil
}

// step runs run/step_into/step_over/step_out/break and reports the new location.
func (s *session) step(cmd string) (string, error) {
	if cmd == "break" {
		s.mu.Lock()
		async := s.asyncBreak
		s.mu.Unlock()
		if !async {
			return "", fmt.Errorf("engine does not support async break (supports_async=0)")
		}
	}
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
	if len(r.Props) == 0 {
		return "(no result)", nil
	}
	return summarize(r.Props[0]), nil
}

func (s *session) PropertyGet(name string, depth int) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("name must not contain NUL")
	}
	r, _, err := s.cmd("property_get", fmt.Sprintf("-d %d -n %s", depth, quoteArg(name)))
	if err != nil {
		return "", err
	}
	if len(r.Props) == 0 {
		return "(not found)", nil
	}
	return summarize(r.Props[0]), nil
}

func (s *session) PropertySet(name, value string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("name must not contain NUL")
	}
	enc := base64.StdEncoding.EncodeToString([]byte(value))
	r, _, err := s.cmd("property_set", fmt.Sprintf("-n %s -- %s", quoteArg(name), enc))
	if err != nil {
		return "", err
	}
	if r.Success == "0" {
		return "", fmt.Errorf("property_set failed: success=\"0\"")
	}
	return fmt.Sprintf("%s = %s", name, value), nil
}

// controlTimeout bounds the best-effort detach/stop command so Detach/Stop
// return even when a run is pending and the engine is not reading commands.
const controlTimeout = 500 * time.Millisecond

func (s *session) Detach() (string, error) {
	if _, _, err := s.cmdWithin("detach", "", controlTimeout); err != nil {
		log.Printf("detach: %v", err) // best-effort: the engine may be mid-run
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked()
	return "detached", nil
}

func (s *session) Stop() (string, error) {
	if _, _, err := s.cmdWithin("stop", "", controlTimeout); err != nil {
		log.Printf("stop: %v", err) // best-effort: the engine may be mid-run
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked()
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
	fullCmd := s.containerExec + " " + cmd
	c := exec.Command("sh", "-c", fullCmd)
	c.Dir = s.projectDir
	out, err := c.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("%s: %w", text, err)
	}
	return text, nil
}

// --- waiting for a connection ----------------------------------------------

// waitForSession blocks until adopt finishes (ready) or the engine fails to
// connect. acceptResult carries the listener's Accept error (nil once a
// connection was accepted, before the handshake). interrupt, when non-nil,
// carries a failure of the triggering request or command. On success it returns
// nil; otherwise it returns the handshake error or a timeout error. It always
// closes the listener before returning, so a later listen never sees a stale
// listener, and it never leaves an orphan paused session behind.
func (s *session) waitForSession(ready <-chan struct{}, acceptResult <-chan error, interrupt <-chan error, timeout time.Duration, noEngineFmt string) error {
	err := s.awaitOutcome(ready, acceptResult, interrupt, timeout, noEngineFmt)
	s.closeLn()
	return err
}

func (s *session) awaitOutcome(ready <-chan struct{}, acceptResult <-chan error, interrupt <-chan error, timeout time.Duration, noEngineFmt string) error {
	select {
	case <-ready:
		return s.handshakeError()
	case err := <-acceptResult:
		if err != nil {
			return fmt.Errorf(noEngineFmt, timeout)
		}
		return s.awaitHandshake(ready)
	case err := <-interrupt:
		return s.requestErrorUnlessReady(ready, acceptResult, err)
	}
}

// awaitHandshake waits for adopt to finish after Xdebug connected. adopt bounds
// the handshake with handshakeTimeout, so this wait is bounded too; the grace
// only covers scheduler delay. If adopt still has not finished, the accepted
// connection is dropped so no orphan paused session remains.
func (s *session) awaitHandshake(ready <-chan struct{}) error {
	select {
	case <-ready:
		return s.handshakeError()
	case <-time.After(handshakeTimeout + handshakeGrace):
		s.dropOrphan(ready)
		return fmt.Errorf("xdebug connected but the DBGp handshake did not finish within %s", handshakeTimeout)
	}
}

// dropOrphan drops a connection adopt is still handshaking so a caller timeout
// cannot leave a paused session the client does not know about. It keeps the
// session when adopt finished in the meantime. The ready check runs under mu,
// which adopt holds for the whole handshake, so adopt cannot finish between the
// check and the drop.
func (s *session) dropOrphan(ready <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-ready:
		return
	default:
	}
	if s.acceptedConn != nil {
		s.acceptedConn.Close()
		s.acceptedConn = nil
	}
	if s.conn != nil {
		s.dropLocked()
	}
}

// ListenWait opens the DBGp port, blocks until the next engine connection is
// adopted, then closes the port. Use for CLI/Symfony commands launched separately.
func (s *session) ListenWait(timeout time.Duration) (string, error) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()

	acceptResult, err := s.openOnce(timeout, 10*time.Second)
	if err != nil {
		return "", err
	}
	if err := s.waitForSession(ready, acceptResult, nil, timeout, "no engine connected within %s"); err != nil {
		return "", err
	}
	return s.Status(), nil
}

// ListenFireForget opens the DBGp port and returns immediately — the listener
// stays open and the next Xdebug connection will be adopted. Use when the
// caller wants to arm the listener and then trigger the command separately
// (e.g. via run_command) without blocking on listen. Check status
// later to see if a session was adopted.
func (s *session) ListenFireForget() (string, error) {
	// We don't know the accept timeout here — use a long default (1h) so the
	// listener stays open. It'll be closed when adopt() runs.
	if _, err := s.openOnce(time.Hour, 10*time.Second); err != nil {
		return "", err
	}
	return "listener armed (fire-and-forget); check status to see if a session was adopted", nil
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

	acceptResult, err := s.openOnce(timeout, 10*time.Second)
	if err != nil {
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

	if err := s.waitForSession(ready, acceptResult, nil, timeout, "no Xdebug connection within %s — is Xdebug enabled in the container? (docker compose exec php set-xdebug-on)"); err != nil {
		return "", err
	}
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
}
