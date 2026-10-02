package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
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
