package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGetStr(t *testing.T) {
	args := map[string]any{"s": "abc", "n": 1.0, "empty": ""}
	for _, c := range []struct{ key, want string }{
		{"s", "abc"}, {"n", ""}, {"empty", ""}, {"missing", ""},
	} {
		if got := getStr(args, c.key); got != c.want {
			t.Errorf("getStr(%q) = %q, want %q", c.key, got, c.want)
		}
	}
	if got := getStr(nil, "s"); got != "" {
		t.Errorf("getStr(nil) = %q, want empty", got)
	}
}

func TestGetInt(t *testing.T) {
	args := map[string]any{
		"float": 42.0, "fraction": 2.9, "int": 7, "str": "15", "badstr": "x",
		"bool": true, "neg": -3.0,
	}
	for _, c := range []struct {
		key  string
		want int
	}{
		{"float", 42}, {"fraction", 2}, {"int", 7}, {"str", 15}, {"badstr", 0},
		{"bool", 0}, {"neg", -3}, {"missing", 0},
	} {
		if got := getInt(args, c.key); got != c.want {
			t.Errorf("getInt(%q) = %d, want %d", c.key, got, c.want)
		}
	}
}

func TestGetStrMap(t *testing.T) {
	args := map[string]any{
		"h":     map[string]any{"A": "1", "B": 2.0, "C": nil},
		"notmp": "x",
	}
	if got, want := getStrMap(args, "h"), map[string]string{"A": "1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("getStrMap(h) = %v, want %v", got, want)
	}
	for _, k := range []string{"notmp", "missing"} {
		got := getStrMap(args, k)
		if got == nil || len(got) != 0 {
			t.Errorf("getStrMap(%q) = %#v, want an empty non-nil map", k, got)
		}
	}
}

func TestHandleInitialize(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "initialize"})
	res, ok := resp.Result.(map[string]any)
	if resp.Error != nil || !ok {
		t.Fatalf("got %+v", resp)
	}
	if res["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities has no tools: %v", res["capabilities"])
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	resp := m.handle(rpcReq{ID: json.RawMessage(`5`), Method: "nope/nope"})
	if resp.Error == nil || resp.Error.Code != -32601 || string(resp.ID) != "5" {
		t.Fatalf("got %+v, want error -32601 with id 5", resp)
	}
	if !strings.Contains(resp.Error.Message, "nope/nope") {
		t.Errorf("message %q does not name the method", resp.Error.Message)
	}
}

func TestHandleNotificationNoReply(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	if resp := m.handle(rpcReq{Method: "tools/list"}); resp != nil {
		t.Fatalf("a request without id must get no reply, got %+v", resp)
	}
}

func TestHandleToolsList(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	resp := m.handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/list"})
	res, _ := resp.Result.(map[string]any)
	tools, _ := res["tools"].([]mcpTool)
	if len(tools) != 21 {
		t.Fatalf("got %d tools, want 21", len(tools))
	}
	seen := map[string]bool{}
	for _, tl := range tools {
		if seen[tl.Name] {
			t.Errorf("duplicate tool %q", tl.Name)
		}
		seen[tl.Name] = true
		if tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("tool %q: empty description or schema type %v", tl.Name, tl.InputSchema["type"])
		}
	}
}

func TestCallUnknownTool(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	_, err := m.call("no_such_tool", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown tool "no_such_tool"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestCallWithoutSession(t *testing.T) {
	m := newMCP(newSession("/l", "/d"))
	for _, tool := range []string{"run", "step_into", "step_over", "step_out", "pause", "stack", "context", "eval", "property_get", "property_set"} {
		t.Run(tool, func(t *testing.T) {
			_, err := m.call(tool, map[string]any{"expression": "1", "name": "$x", "value": "1"})
			if err == nil || !strings.Contains(err.Error(), "no active session") {
				t.Fatalf("err = %v, want no active session", err)
			}
		})
	}
}

func TestCallSendsDBGpCommand(t *testing.T) {
	for _, c := range []struct {
		tool string
		args map[string]any
		want string // the DBGp command line the engine receives
	}{
		{"run", nil, "run -i 1"},
		{"step_into", nil, "step_into -i 1"},
		{"step_over", nil, "step_over -i 1"},
		{"step_out", nil, "step_out -i 1"},
		{"pause", nil, "break -i 1"},
		{"stack", nil, "stack_get -i 1"},
		{"context", nil, "context_get -i 1 -d 0"},
		{"context", map[string]any{"stackDepth": 2.0}, "context_get -i 1 -d 2"},
		{"eval", map[string]any{"expression": "1+1"}, "eval -i 1 -- MSsx"},
		{"property_get", map[string]any{"name": "$x", "stackDepth": "1"}, "property_get -i 1 -d 1 -n \"$x\""},
		{"property_set", map[string]any{"name": "$x", "value": "5"}, "property_set -i 1 -n \"$x\" -- NQ=="},
		{"set_breakpoint", map[string]any{"file": "src/A.php", "line": 3.0}, "breakpoint_set -i 1 -t line -f \"file:///d/src/A.php\" -n 3"},
	} {
		t.Run(c.tool+" "+c.want, func(t *testing.T) {
			s, eng := newActiveSession(t, "break")
			got := eng.answer(xmlProlog + `<response command="x" transaction_id="1" status="break" id="1"/>`)
			if _, err := newMCP(s).call(c.tool, c.args); err != nil {
				t.Fatalf("call: %v", err)
			}
			if cmd := <-got; cmd != c.want {
				t.Fatalf("engine got %q, want %q", cmd, c.want)
			}
		})
	}
}

func TestCallToolsWithoutEngine(t *testing.T) {
	for _, c := range []struct {
		tool    string
		args    map[string]any
		want    string // expected text, or a part of the error message
		wantErr bool
	}{
		{"status", nil, "state=no session\nlocation=-\nbreakpoints=0", false},
		{"breakpoint_clear", nil, "cleared 0 breakpoint(s)", false},
		{"detach", nil, "detached", false},
		{"stop", nil, "stopped", false},
		{"request", map[string]any{}, "url required", true},
		{"request_from_files", map[string]any{}, "url required", true},
		{"run_command", map[string]any{}, "command required", true},
		{"run_command", map[string]any{"command": "ls"}, "container-exec not configured", true},
		{"listen", map[string]any{"timeoutMs": 50.0}, "no engine connected within 50ms", true},
		{"container_status", nil, "command not configured", true},
		{"container_enable", nil, "command not configured", true},
		{"container_disable", nil, "command not configured", true},
	} {
		t.Run(c.tool, func(t *testing.T) {
			s := newSession("/l", "/d")
			s.dbgAddr = "127.0.0.1:0" // listen: let the kernel pick a free port
			out, err := newMCP(s).call(c.tool, c.args)
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want it to contain %q", err, c.want)
				}
				return
			}
			if err != nil || out != c.want {
				t.Fatalf("call = %q, %v; want %q", out, err, c.want)
			}
		})
	}
}
