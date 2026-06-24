package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
		{"docker_xdebug_status", "Current debug session state and location.", obj(nil)},
		{"docker_xdebug_set_breakpoint", "Set a line breakpoint. `file` is a HOST path (absolute or project-relative); it is auto-translated to the container path. Queued if no session is active yet.", obj(map[string]any{"file": prop("string", "host path, e.g. src/Foo/Bar.php"), "line": prop("integer", "1-based line")}, "file", "line")},
		{"docker_xdebug_breakpoint_list", "List breakpoints (locations shown as host paths).", obj(nil)},
		{"docker_xdebug_breakpoint_remove", "Remove a breakpoint by id.", obj(map[string]any{"id": prop("string", "breakpoint id")}, "id")},
		{"docker_xdebug_breakpoint_clear", "Clear ALL breakpoints (queued and applied). Safe with or without an active session.", obj(nil)},
		{"docker_xdebug_request", "Fire an HTTP request at the app (any method, headers, body) and run to completion. Does NOT pause at breakpoints — use docker_xdebug_listen first, then trigger the request separately to debug interactively.", obj(map[string]any{
			"url":       prop("string", "full URL, e.g. http://127.0.0.1:8090/api/foo"),
			"method":    prop("string", "HTTP method (default GET)"),
			"headers":   strMap,
			"body":      prop("string", "raw request body (e.g. JSON)"),
			"timeoutMs": prop("integer", "max wait for the Xdebug connection (default 15000)"),
		}, "url")},
		{"docker_xdebug_request_files", "Like docker_xdebug_request but reads headers and body from files on disk. Use this when headers contain sensitive values (JWT tokens, cookies) that should not appear inline. headers_file: path to a text file with \"Name: Value\" lines (blank lines and # comments ignored). body_file: path to raw body bytes.", obj(map[string]any{
			"url":          prop("string", "full URL, e.g. http://127.0.0.1:8090/api/foo"),
			"method":       prop("string", "HTTP method (default GET)"),
			"headers_file": prop("string", "path to headers file (JSON or Name: Value lines)"),
			"body_file":    prop("string", "path to body file (raw bytes)"),
			"timeoutMs":    prop("integer", "max wait for the Xdebug connection (default 15000)"),
		}, "url")},
		{"docker_xdebug_listen", "Arm the listener and wait for the NEXT engine connection — use for CLI/Symfony commands launched separately.", obj(map[string]any{"timeoutMs": prop("integer", "max wait (default 30000)")})},
		{"docker_xdebug_run_command", "Run a command inside the container (e.g. a Symfony console command) and wait for the Xdebug connection. Like docker_xdebug_request but for CLI commands. When no breakpoints are set, the script runs to completion and the command output is returned. When breakpoints are set, the session pauses — call run/step to drive.", obj(map[string]any{
			"command":   prop("string", "command to run in the container, e.g. \"bin/console app:my-command --option=value\""),
			"timeoutMs": prop("integer", "max wait for the Xdebug connection (default 30000)"),
		}, "command")},
		{"docker_xdebug_run", "Resume to the next breakpoint or end.", obj(nil)},
		{"docker_xdebug_step_into", "Step into.", obj(nil)},
		{"docker_xdebug_step_over", "Step over.", obj(nil)},
		{"docker_xdebug_step_out", "Step out.", obj(nil)},
		{"docker_xdebug_pause", "Break/pause execution.", obj(nil)},
		{"docker_xdebug_stack", "Call stack (host paths).", obj(nil)},
		{"docker_xdebug_context", "Variables in scope.", obj(map[string]any{"stackDepth": prop("integer", "stack frame, default 0")})},
		{"docker_xdebug_eval", "Evaluate a PHP expression in the current scope.", obj(map[string]any{"expression": prop("string", "PHP expression")}, "expression")},
		{"docker_xdebug_property_get", "Get one variable/property by name.", obj(map[string]any{"name": prop("string", "variable name, e.g. $foo"), "stackDepth": prop("integer", "stack frame, default 0")}, "name")},
		{"docker_xdebug_property_set", "Set a variable to a PHP literal value.", obj(map[string]any{"name": prop("string", "variable name"), "value": prop("string", "PHP value")}, "name", "value")},
		{"docker_xdebug_detach", "Detach: let the script finish, drop the session.", obj(nil)},
		{"docker_xdebug_stop", "Stop: terminate the debugged script.", obj(nil)},
	}
	if s.statusCmd != "" {
		t = append(t, mcpTool{"docker_xdebug_container_status", "Check whether Xdebug is enabled in the container.", obj(nil)})
	}
	if s.enableCmd != "" {
		t = append(t, mcpTool{"docker_xdebug_container_enable", "Enable Xdebug in the container.", obj(nil)})
	}
	if s.disableCmd != "" {
		t = append(t, mcpTool{"docker_xdebug_container_disable", "Disable Xdebug in the container.", obj(nil)})
	}
	return &mcpServer{sess: s, tools: t}
}

func (m *mcpServer) serve() {
	rd := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := json.NewEncoder(os.Stdout)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var req rpcReq
			if json.Unmarshal(line, &req) == nil {
				if resp := m.handle(req); resp != nil {
					out.Encode(resp) // Encode appends a newline
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
			"serverInfo":      map[string]any{"name": "docker-xdebug", "version": "0.1.0"},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": m.tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
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
	case "docker_xdebug_status":
		return s.Status(), nil
	case "docker_xdebug_set_breakpoint":
		return s.SetBreakpoint(getStr(a, "file"), getInt(a, "line"))
	case "docker_xdebug_breakpoint_list":
		return s.BreakpointList()
	case "docker_xdebug_breakpoint_remove":
		return s.BreakpointRemove(getStr(a, "id"))
	case "docker_xdebug_breakpoint_clear":
		return s.BreakpointClearAll()
	case "docker_xdebug_request":
		return s.DoRequest(getStr(a, "url"), getStr(a, "method"), getStrMap(a, "headers"), getStr(a, "body"), time.Duration(getInt(a, "timeoutMs"))*time.Millisecond)
	case "docker_xdebug_request_files":
		return s.DoRequestFromFiles(getStr(a, "url"), getStr(a, "method"), getStr(a, "headers_file"), getStr(a, "body_file"), time.Duration(getInt(a, "timeoutMs"))*time.Millisecond)
	case "docker_xdebug_listen":
		t := getInt(a, "timeoutMs")
		if t == 0 {
			t = 30000
		}
		return s.ListenWait(time.Duration(t) * time.Millisecond)
	case "docker_xdebug_run_command":
		t := getInt(a, "timeoutMs")
		if t == 0 {
			t = 30000
		}
		return s.RunCommand(getStr(a, "command"), time.Duration(t)*time.Millisecond)
	case "docker_xdebug_run":
		return s.step("run")
	case "docker_xdebug_step_into":
		return s.step("step_into")
	case "docker_xdebug_step_over":
		return s.step("step_over")
	case "docker_xdebug_step_out":
		return s.step("step_out")
	case "docker_xdebug_pause":
		return s.step("break")
	case "docker_xdebug_stack":
		return s.Stack()
	case "docker_xdebug_context":
		return s.Context(getInt(a, "stackDepth"))
	case "docker_xdebug_eval":
		return s.Eval(getStr(a, "expression"))
	case "docker_xdebug_property_get":
		return s.PropertyGet(getStr(a, "name"), getInt(a, "stackDepth"))
	case "docker_xdebug_property_set":
		return s.PropertySet(getStr(a, "name"), getStr(a, "value"))
	case "docker_xdebug_detach":
		return s.Detach()
	case "docker_xdebug_stop":
		return s.Stop()
	case "docker_xdebug_container_status":
		return s.XdebugContainerStatus()
	case "docker_xdebug_container_enable":
		return s.XdebugEnable()
	case "docker_xdebug_container_disable":
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
