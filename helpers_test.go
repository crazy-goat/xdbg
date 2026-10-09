package main

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// answer reads one command, replies with xml, and returns the complete command
// including the transaction id. The result channel is buffered so the engine
// goroutine can finish even if the test fails before receiving from it.
func (e *fakeEngine) answer(xml string) <-chan string {
	ch := make(chan string, 1)
	go func() {
		command, tx := e.readCmd()
		e.send(withTx(xml, tx))

		parts := strings.SplitN(command, " ", 2)
		command = parts[0] + " -i " + tx
		if len(parts) == 2 {
			command += " " + parts[1]
		}
		ch <- command
	}()
	return ch
}

// newActiveSession returns a session connected to a fake engine and in state.
func newActiveSession(t *testing.T, state string) (*session, *fakeEngine) {
	t.Helper()
	eng, conn := newPipe(t)
	s := newSession("/l", "/d")
	s.conn = conn
	s.r = bufio.NewReader(conn)
	s.state = state
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	s.startReader(conn, s.r)
	return s, eng
}
