package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunShellNotConfigured checks that each optional container command reports
// an actionable error when it has not been configured.
func TestRunShellNotConfigured(t *testing.T) {
	s := newSession("/l", "/d")
	for name, f := range map[string]func() (string, error){
		"enable": s.XdebugEnable, "disable": s.XdebugDisable, "status": s.XdebugContainerStatus,
	} {
		if _, err := f(); err == nil || !strings.Contains(err.Error(), "command not configured") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestContainerCommandsRunTheirShellCommand covers successful execution of the
// configured enable, disable, and status commands.
func TestContainerCommandsRunTheirShellCommand(t *testing.T) {
	s := newSession("/l", "/d")
	s.enableCmd, s.disableCmd, s.statusCmd = "echo on", "echo off", "echo '  status: on  '"
	for _, c := range []struct {
		name string
		f    func() (string, error)
		want string
	}{
		{"enable", s.XdebugEnable, "on"},
		{"disable", s.XdebugDisable, "off"},
		{"status", s.XdebugContainerStatus, "status: on"}, // output is trimmed
	} {
		if got, err := c.f(); err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// TestRunShellPrefixesContainerExec pins the container-exec prefix behavior.
func TestRunShellPrefixesContainerExec(t *testing.T) {
	s := newSession("/l", "/d")
	s.containerExec = "echo PREFIX"
	s.statusCmd = "get-status"
	if got, err := s.XdebugContainerStatus(); err != nil || got != "PREFIX get-status" {
		t.Fatalf("got %q, %v; want %q", got, err, "PREFIX get-status")
	}
}

// TestRunShellRunsInProjectDir checks the working directory used for commands.
func TestRunShellRunsInProjectDir(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newSession("/l", "/d")
	s.projectDir = dir
	s.statusCmd = "pwd -P"
	if got, err := s.XdebugContainerStatus(); err != nil || got != dir {
		t.Fatalf("got %q, %v; want %q", got, err, dir)
	}
}

// TestRunShellFailureReturnsOutputAndExitCode checks errors retain command output
// and the exit status.
func TestRunShellFailureReturnsOutputAndExitCode(t *testing.T) {
	s := newSession("/l", "/d")
	s.enableCmd = "echo boom >&2; exit 3"
	_, err := s.XdebugEnable()
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
}

// TestNewMCPContainerTools checks optional container tool registration.
func TestNewMCPContainerTools(t *testing.T) {
	names := func(m *mcpServer) map[string]bool {
		out := map[string]bool{}
		for _, tl := range m.tools {
			out[tl.Name] = true
		}
		return out
	}
	s := newSession("/l", "/d")
	got := names(newMCP(s))
	for _, n := range []string{"container_status", "container_enable", "container_disable"} {
		if got[n] {
			t.Errorf("%s listed without its flag", n)
		}
	}
	s.statusCmd = "x"
	got = names(newMCP(s))
	if !got["container_status"] || got["container_enable"] || got["container_disable"] {
		t.Errorf("only container_status expected: %v", got)
	}
	s.enableCmd, s.disableCmd = "x", "x"
	if got = names(newMCP(s)); !got["container_enable"] || !got["container_disable"] || len(got) != 24 {
		t.Errorf("want all 24 tools, got %d", len(got))
	}
}

// TestCallContainerStatusTool checks the successful tools/call response.
func TestCallContainerStatusTool(t *testing.T) {
	s := newSession("/l", "/d")
	s.statusCmd = "echo xdebug is on"
	resp := newMCP(s).handle(rpcReq{ID: json.RawMessage(`1`), Method: "tools/call", Params: json.RawMessage(`{"name":"container_status"}`)})
	b, _ := json.Marshal(resp.Result)
	if resp.Error != nil || !strings.Contains(string(b), `"text":"xdebug is on"`) || strings.Contains(string(b), "isError") {
		t.Fatalf("got %s / %+v", b, resp.Error)
	}
}

// TestRunCommandValidation checks required command configuration.
func TestRunCommandValidation(t *testing.T) {
	s := newSession("/l", "/d")
	if _, err := s.RunCommand("", time.Second); err == nil || err.Error() != "command required" {
		t.Errorf("empty command: err = %v", err)
	}
	if _, err := s.RunCommand("bin/console x", time.Second); err == nil || !strings.Contains(err.Error(), "container-exec not configured") {
		t.Errorf("no container exec: err = %v", err)
	}
}

// TestRunCommandNoXdebugTimeout checks listener cleanup after the connection timeout.
func TestRunCommandNoXdebugTimeout(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = "127.0.0.1:0"
	s.containerExec = "sleep 1 #"
	_, err := s.RunCommand("bin/console x", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no Xdebug connection within 300ms") {
		t.Fatalf("err = %v", err)
	}
	if listenerAddr(s) != "" {
		t.Fatal("the listener must be closed after the timeout")
	}
}

// TestRunCommandReturnsOutput checks that a fake DBGp engine can complete a
// command without Docker and its output is returned.
func TestRunCommandReturnsOutput(t *testing.T) {
	s := newSession("/l", "/d")
	s.dbgAddr = "127.0.0.1:0"
	s.containerExec = "sleep 0.5; echo"
	engineDone := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if addr := listenerAddr(s); addr != "" {
				engineDone <- dialCommandEngine(addr)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		engineDone <- nil
	}()
	out, err := s.RunCommand("hello world", 5*time.Second)
	if engineErr := <-engineDone; engineErr != nil {
		t.Fatalf("engine: %v", engineErr)
	}
	if err != nil || out != "hello world" {
		t.Fatalf("RunCommand() = %q, %v", out, err)
	}
}
