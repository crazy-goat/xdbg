package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// dialCommandEngine plays Xdebug over TCP: it dials addr, sends <init>, then answers
// each command. `run` gets status="stopping", every other command a plain
// response. It returns when the debugger closes the connection.
func dialCommandEngine(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(dbgpPacket(xmlProlog + `<init fileuri="file:///d/index.php"/>`))); err != nil {
		return err
	}
	r := bufio.NewReader(c)
	for tx := 1; ; tx++ {
		cmd, err := r.ReadString(0)
		if err != nil {
			return nil // debugger closed the connection: done
		}
		name := strings.Fields(cmd)[0]
		status := "break"
		if name == "run" {
			status = "stopping"
		}
		reply := fmt.Sprintf(`<response command="%s" transaction_id="%d" status="%s" id="1"/>`, name, tx, status)
		if _, err := c.Write([]byte(dbgpPacket(xmlProlog + reply))); err != nil {
			return nil
		}
	}
}

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
