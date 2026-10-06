package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // empty => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpServer struct {
	sess  *session
	tools []mcpTool
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if props == nil {
		m["properties"] = map[string]any{}
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

func newMCP(s *session) *mcpServer {
	strMap := map[string]any{"type": "object", "description": "HTTP headers", "additionalProperties": map[string]any{"type": "string"}}
	t := []mcpTool{
		{"status", "Current debug session state and location.", obj(nil)},
		{"set_breakpoint", "Set a line breakpoint. `file` is a HOST path (absolute or project-relative); it is auto-translated to the container path. Queued if no session is active yet.", obj(map[string]any{"file": prop("string", "host path, e.g. src/Foo/Bar.php"), "line": prop("integer", "1-based line")}, "file", "line")},
		{"breakpoint_list", "List engine, queued, and rejected breakpoints (host paths). Safe with or without an active session.", obj(nil)},
		{"breakpoint_remove", "Remove a breakpoint by id.", obj(map[string]any{"id": prop("string", "breakpoint id")}, "id")},
		{"breakpoint_clear", "Clear ALL breakpoints (queued and applied). Safe with or without an active session.", obj(nil)},
		{"request", "Fire an HTTP request at the app (any method, headers, body) and run to completion. Does NOT pause at breakpoints — use listen first, then trigger the request separately to debug interactively.", obj(map[string]any{
			"url":       prop("string", "full URL, e.g. http://127.0.0.1:8090/api/foo"),
			"method":    prop("string", "HTTP method (default GET)"),
			"headers":   strMap,
			"body":      prop("string", "raw request body (e.g. JSON)"),
			"timeoutMs": prop("integer", "max wait for the Xdebug connection (default 15000)"),
		}, "url")},
		{"request_from_files", "Like request but reads headers and body from files on disk. Use files to keep sensitive header values (JWT tokens, cookies) out of tool arguments. headers_file: a JSON object with string values (one line or multiple lines), or \"Name: Value\" lines. The line format ignores blank lines and lines that start with #. Header names in the line format must use RFC 7230 token characters. Host sets the request Host, regardless of case. body_file: raw request body bytes.", obj(map[string]any{
			"url":          prop("string", "full URL, e.g. http://127.0.0.1:8090/api/foo"),
			"method":       prop("string", "HTTP method (default GET)"),
			"headers_file": prop("string", "path to a JSON object with string values (one line or multiple lines), or Name: Value lines; the line format ignores blank lines and lines that start with #, and requires RFC 7230 token names; Host sets the request Host, regardless of case"),
			"body_file":    prop("string", "path to body file (raw bytes)"),
			"timeoutMs":    prop("integer", "max wait for the Xdebug connection (default 15000)"),
		}, "url")},
		{"listen", "Arm the listener and wait for the NEXT engine connection — use for CLI/Symfony commands launched separately.", obj(map[string]any{"timeoutMs": prop("integer", "max wait (default 30000)")})},
		{"run_command", "Run a command inside the container (e.g. a Symfony console command) and wait for the Xdebug connection. Like request but for CLI commands. When no breakpoints are set, the script runs to completion and the command output is returned. When breakpoints are set, the session pauses — call run/step to drive.", obj(map[string]any{
			"command":   prop("string", "command to run in the container, e.g. \"bin/console app:my-command --option=value\""),
			"timeoutMs": prop("integer", "max wait for the Xdebug connection (default 30000)"),
		}, "command")},
		{"run", "Resume to the next breakpoint or end.", obj(nil)},
		{"step_into", "Step into.", obj(nil)},
		{"step_over", "Step over.", obj(nil)},
		{"step_out", "Step out.", obj(nil)},
		{"pause", "Break/pause execution.", obj(nil)},
		{"stack", "Call stack (host paths).", obj(nil)},
		{"context", "Variables in scope.", obj(map[string]any{"stackDepth": prop("integer", "stack frame, default 0")})},
		{"eval", "Evaluate a PHP expression in the current scope.", obj(map[string]any{"expression": prop("string", "PHP expression")}, "expression")},
		{"property_get", "Get one variable/property by name.", obj(map[string]any{"name": prop("string", "variable name, e.g. $foo"), "stackDepth": prop("integer", "stack frame, default 0")}, "name")},
		{"property_set", "Set a variable to a PHP literal value.", obj(map[string]any{"name": prop("string", "variable name"), "value": prop("string", "PHP value")}, "name", "value")},
		{"detach", "Detach: let the script finish, drop the session.", obj(nil)},
		{"stop", "Stop: terminate the debugged script.", obj(nil)},
	}
	if s.statusCmd != "" {
		t = append(t, mcpTool{"container_status", "Check whether Xdebug is enabled in the container.", obj(nil)})
	}
	if s.enableCmd != "" {
		t = append(t, mcpTool{"container_enable", "Enable Xdebug in the container.", obj(nil)})
	}
	if s.disableCmd != "" {
		t = append(t, mcpTool{"container_disable", "Disable Xdebug in the container.", obj(nil)})
	}
	return &mcpServer{sess: s, tools: t}
}

func (m *mcpServer) serve() { m.serveIO(os.Stdin, os.Stdout) }

// serveIO runs the JSON-RPC loop: one request per line from in, one response per line to out.
func (m *mcpServer) serveIO(in io.Reader, w io.Writer) {
	rd := bufio.NewReaderSize(in, 1<<20)
	out := json.NewEncoder(w)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var req rpcReq
			var resp *rpcResp
			if uerr := json.Unmarshal(line, &req); uerr != nil {
				// No usable id: JSON-RPC 2.0 wants a null id. Syntax errors are parse
				// errors; valid JSON of the wrong shape is an invalid request.
				code, msg := -32700, "parse error: "
				if json.Valid(line) {
					code, msg = -32600, "invalid request: "
				}
				log.Printf("%s%v", msg, uerr)
				resp = &rpcResp{JSONRPC: "2.0", Error: &rpcErr{Code: code, Message: msg + uerr.Error()}}
			} else if req.Method == "" {
				// Valid JSON that decodes to a request without a method, e.g. `{}` or `null`.
				// Without this check it would pass for a notification and get no reply.
				log.Printf("invalid request: missing method")
				resp = &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32600, Message: "invalid request: missing method"}}
			} else {
				resp = m.handle(req)
			}
			if resp != nil {
				// Encode appends the newline. The client may be gone; there is nobody to tell, so log.
				if err := out.Encode(resp); err != nil {
					log.Printf("write response: %v", err)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *mcpServer) handle(req rpcReq) *rpcResp {
	if len(req.ID) == 0 {
		return nil // notification (e.g. notifications/initialized) — no reply
	}
	resp := &rpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "xdbg", "version": version},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": m.tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcErr{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		text, err := m.call(p.Name, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{"content": []any{textContent(err.Error())}, "isError": true}
		} else {
			resp.Result = map[string]any{"content": []any{textContent(text)}}
		}
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcErr{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func (m *mcpServer) call(name string, a map[string]any) (string, error) {
	s := m.sess
	switch name {
	case "status":
		return s.Status(), nil
	case "set_breakpoint":
		return s.SetBreakpoint(getStr(a, "file"), getInt(a, "line"))
	case "breakpoint_list":
		return s.BreakpointList()
	case "breakpoint_remove":
		return s.BreakpointRemove(getStr(a, "id"))
	case "breakpoint_clear":
		return s.BreakpointClearAll()
	case "request":
		return s.DoRequest(getStr(a, "url"), getStr(a, "method"), getStrMap(a, "headers"), getStr(a, "body"), time.Duration(getInt(a, "timeoutMs"))*time.Millisecond)
	case "request_from_files":
		return s.DoRequestFromFiles(getStr(a, "url"), getStr(a, "method"), getStr(a, "headers_file"), getStr(a, "body_file"), time.Duration(getInt(a, "timeoutMs"))*time.Millisecond)
	case "listen":
		t := getInt(a, "timeoutMs")
		if t == 0 {
			t = 30000
		}
		return s.ListenWait(time.Duration(t) * time.Millisecond)
	case "run_command":
		t := getInt(a, "timeoutMs")
		if t == 0 {
			t = 30000
		}
		return s.RunCommand(getStr(a, "command"), time.Duration(t)*time.Millisecond)
	case "run":
		return s.step("run")
	case "step_into":
		return s.step("step_into")
	case "step_over":
		return s.step("step_over")
	case "step_out":
		return s.step("step_out")
	case "pause":
		return s.step("break")
	case "stack":
		return s.Stack()
	case "context":
		return s.Context(getInt(a, "stackDepth"))
	case "eval":
		return s.Eval(getStr(a, "expression"))
	case "property_get":
		return s.PropertyGet(getStr(a, "name"), getInt(a, "stackDepth"))
	case "property_set":
		return s.PropertySet(getStr(a, "name"), getStr(a, "value"))
	case "detach":
		return s.Detach()
	case "stop":
		return s.Stop()
	case "container_status":
		return s.XdebugContainerStatus()
	case "container_enable":
		return s.XdebugEnable()
	case "container_disable":
		return s.XdebugDisable()
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func textContent(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func getStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func getInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func getStrMap(m map[string]any, k string) map[string]string {
	out := map[string]string{}
	if mm, ok := m[k].(map[string]any); ok {
		for kk, vv := range mm {
			if s, ok := vv.(string); ok {
				out[kk] = s
			}
		}
	}
	return out
}
