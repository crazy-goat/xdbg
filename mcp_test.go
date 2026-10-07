package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func decodeResponses(t *testing.T, raw string) []rpcResp {
	t.Helper()
	var out []rpcResp
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var r rpcResp
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad response line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func TestToolsCallMalformedParams(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	for name, params := range map[string]string{
		"string params": `"nope"`,
		"array params":  `[1,2]`,
		"bad name type": `{"name":42}`,
		"bad arguments": `{"name":"status","arguments":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(params)})
			if resp.Error == nil || resp.Error.Code != -32602 {
				t.Fatalf("want JSON-RPC error -32602, got %+v", resp)
			}
			if resp.Result != nil {
				t.Fatalf("an error response must carry no result, got %v", resp.Result)
			}
		})
	}
}

func TestToolsCallValidParams(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(`{"name":"status","arguments":{}}`)})
	if resp.Error != nil || resp.Result == nil {
		t.Fatalf("valid call failed: %+v", resp)
	}
}

func TestToolsCallSetBreakpointEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	go eng.respond(`<response command="breakpoint_set"><error code="200"><message>breakpoint could not be set</message></error></response>`)
	m := newMCP(s)

	resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(`{"name":"set_breakpoint","arguments":{"file":"a.php","line":3}}`)})
	if resp == nil || resp.Error != nil {
		t.Fatalf("set_breakpoint must return a tool result, got %+v", resp)
	}
	res, ok := resp.Result.(map[string]any)
	if !ok || res["isError"] != true {
		t.Fatalf("set_breakpoint must return isError: true, got %+v", resp)
	}
	if text := fmt.Sprint(res["content"]); !strings.Contains(text, "breakpoint_set error 200: breakpoint could not be set") {
		t.Fatalf("set_breakpoint result = %v, want the engine code and message", res)
	}
	if len(s.pending) != 0 {
		t.Fatalf("a rejected breakpoint must not be stored, got %+v", s.pending)
	}
}

func TestToolsCallBreakpointRemoveEngineError(t *testing.T) {
	s, eng := newActivePipe(t)
	s.pending = []bp{{file: "/d/a.php", line: 3, id: "1001", qid: "q1"}}
	go eng.respond(`<response command="breakpoint_remove"><error code="205"><message>no such breakpoint</message></error></response>`)
	m := newMCP(s)

	resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(`{"name":"breakpoint_remove","arguments":{"id":"q1"}}`)})
	if resp == nil || resp.Error != nil {
		t.Fatalf("breakpoint_remove must return a tool result, got %+v", resp)
	}
	res, ok := resp.Result.(map[string]any)
	if !ok || res["isError"] != true {
		t.Fatalf("breakpoint_remove must return isError: true, got %+v", resp)
	}
	if text := fmt.Sprint(res["content"]); !strings.Contains(text, "breakpoint_remove error 205: no such breakpoint") {
		t.Fatalf("breakpoint_remove result = %v; want the engine code and message", res)
	}
	if len(s.pending) != 1 || s.pending[0].id != "1001" || s.pending[0].qid != "q1" {
		t.Fatalf("pending = %+v; an engine error must retain the local breakpoint", s.pending)
	}
}

func TestToolsCallBreakpointRemoveEmptyID(t *testing.T) {
	s := newSession("/l", "/d")
	for _, file := range []string{"a.php", "b.php"} {
		if _, err := s.SetBreakpoint(file, 3); err != nil {
			t.Fatal(err)
		}
	}
	m := newMCP(s)

	for _, arguments := range []string{`{}`, `{"id":""}`} {
		resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(`{"name":"breakpoint_remove","arguments":` + arguments + `}`)})
		if resp == nil || resp.Error != nil {
			t.Fatalf("breakpoint_remove must return a tool result, got %+v", resp)
		}
		res, ok := resp.Result.(map[string]any)
		if !ok || res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "id required") {
			t.Fatalf("breakpoint_remove result = %v; want an id required tool error", resp)
		}
		if len(s.pending) != 2 {
			t.Fatalf("pending = %+v; an empty or missing id must not change state", s.pending)
		}
	}
}

func TestToolsCallListenBadInit(t *testing.T) {
	for _, length := range []string{"-1", "999999999999999", "9999999999"} {
		t.Run(length, func(t *testing.T) {
			probe, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Skipf("cannot listen: %v", err)
			}
			addr := probe.Addr().String()
			probe.Close()

			s := newSession("/l", "/d")
			s.dbgAddr = addr
			t.Cleanup(s.closeLn)
			done := make(chan struct{})
			defer close(done)
			go func() {
				for i := 0; i < 100; i++ {
					if c, err := net.Dial("tcp", addr); err == nil {
						defer c.Close()
						_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
						_, _ = c.Write([]byte(length + "\x00"))
						<-done
						return
					}
					select {
					case <-done:
						return
					case <-time.After(50 * time.Millisecond):
					}
				}
			}()

			m := newMCP(s)
			start := time.Now()
			resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(`{"name":"listen","arguments":{"timeoutMs":10000}}`)})
			if resp == nil || resp.Error != nil {
				t.Fatalf("listen must return a tool result, got %+v", resp)
			}
			res, ok := resp.Result.(map[string]any)
			if !ok || res["isError"] != true {
				t.Fatalf("listen must return an error result, got %+v", resp)
			}
			text := fmt.Sprint(res["content"])
			if !strings.Contains(text, "handshake failed") || !strings.Contains(text, "read init") || !strings.Contains(text, "bad length "+length) {
				t.Fatalf("listen result = %v, want a handshake error for the bad length", res)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("listen took %s, it must not wait for the timeout", time.Since(start))
			}
		})
	}
}

func TestInitializeServerInfo(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	serverInfo := func() map[string]any {
		t.Helper()
		resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "initialize"})
		if resp.Error != nil || resp.Result == nil {
			t.Fatalf("initialize failed: %+v", resp)
		}
		res, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result is not an object: %T", resp.Result)
		}
		info, ok := res["serverInfo"].(map[string]any)
		if !ok {
			t.Fatalf("serverInfo is not an object: %T", res["serverInfo"])
		}
		return info
	}

	info := serverInfo()
	if info["name"] != "xdbg" {
		t.Fatalf("serverInfo.name = %v, want xdbg", info["name"])
	}
	if info["version"] != resolvedVersion() {
		t.Fatalf("serverInfo.version = %v, want the resolved version %q", info["version"], resolvedVersion())
	}

	// The version is injected at build time (-ldflags -X main.version); the
	// serverInfo must read it rather than a hardcoded string.
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })
	if got := serverInfo()["version"]; got != "1.2.3" {
		t.Fatalf("serverInfo.version = %v, want the injected 1.2.3", got)
	}
}

func TestServeIORepliesPerLine(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":"bad"}` + "\n"
	var out bytes.Buffer
	m.serveIO(strings.NewReader(in), &out)
	got := decodeResponses(t, out.String())
	if len(got) != 2 || got[0].Error != nil || got[1].Error == nil || got[1].Error.Code != -32602 {
		t.Fatalf("unexpected responses: %+v", got)
	}
}

func TestServeIOParseError(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	in := `{"jsonrpc":"2.0","id":1,"method":` + "\n" + // truncated JSON
		"\n" + // blank lines get no reply
		`[1,2]` + "\n" + // valid JSON, not a request object
		`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	var out bytes.Buffer
	m.serveIO(strings.NewReader(in), &out)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d responses, want 3: %q", len(lines), out.String())
	}
	for i, want := range []int{-32700, -32600} {
		var r struct {
			ID    json.RawMessage `json:"id"`
			Error *rpcErr         `json:"error"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &r); err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		if string(r.ID) != "null" || r.Error == nil || r.Error.Code != want {
			t.Fatalf("response %d = %s, want error %d with id null", i, lines[i], want)
		}
	}
	// The server keeps serving after bad input.
	got := decodeResponses(t, lines[2])
	if len(got) != 1 || got[0].Error != nil || string(got[0].ID) != "2" {
		t.Fatalf("ping after garbage: %+v", got)
	}
}

func TestServeIOMissingMethod(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	in := `{}` + "\n" +
		`null` + "\n" +
		`{"jsonrpc":"2.0","id":7}` + "\n" + // id but no method: the reply keeps the id
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" // a real notification: no reply
	var out bytes.Buffer
	m.serveIO(strings.NewReader(in), &out)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d responses, want 3: %q", len(lines), out.String())
	}
	for i, wantID := range []string{"null", "null", "7"} {
		var r struct {
			ID    json.RawMessage `json:"id"`
			Error *rpcErr         `json:"error"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &r); err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		if string(r.ID) != wantID || r.Error == nil || r.Error.Code != -32600 {
			t.Fatalf("response %d = %s, want error -32600 with id %s", i, lines[i], wantID)
		}
	}
}

type failingWriter struct{ calls int }

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("broken pipe")
}

func TestServeIOLogsWriteErrors(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	w := &failingWriter{}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	m.serveIO(strings.NewReader(in), w)
	if !strings.Contains(logs.String(), "write response: broken pipe") {
		t.Fatalf("write error was not logged: %q", logs.String())
	}
	// json.Encoder keeps its first error, so only one write reaches the writer. The point is
	// that serveIO logs the failure and returns at EOF instead of panicking or hanging.
	if w.calls < 1 {
		t.Fatalf("want the first response attempted, got %d writes", w.calls)
	}
}
