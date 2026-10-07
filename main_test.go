package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

// executeHelp runs the command tree with args plus --help. --help stops before
// RunE, so the MCP server does not start. It returns the dbg-port value.
func executeHelp(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--help"))
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(%q) = %v, output: %s", args, err, out.String())
	}
	return root.PersistentFlags().Lookup("dbg-port").Value.String()
}

func TestRootAcceptsMCPFlags(t *testing.T) {
	if got := executeHelp(t, "--dbg-port", "19903", "--docker-root", "/var/www/html"); got != "19903" {
		t.Errorf("dbg-port = %q, want 19903", got)
	}
}

func TestMCPSubcommandAcceptsFlags(t *testing.T) {
	if got := executeHelp(t, "mcp", "--dbg-port", "19904", "--docker-root", "/var/www/html"); got != "19904" {
		t.Errorf("dbg-port = %q, want 19904", got)
	}
}

func TestDbgPortDefault(t *testing.T) {
	if got := executeHelp(t); got != "9003" {
		t.Errorf("dbg-port = %q, want 9003", got)
	}
}

func TestListenAddrDefaultIsBindAnywhere(t *testing.T) {
	root := newRootCmd()
	flag := root.PersistentFlags().Lookup("listen-addr")
	if flag == nil {
		t.Fatal("listen-addr is not a persistent flag")
	}
	if got := flag.Value.String(); got != "0.0.0.0" {
		t.Errorf("listen-addr = %q, want 0.0.0.0", got)
	}
}

func TestMCPSessionUsesTheListenAddrAndPort(t *testing.T) {
	s, err := mcpSession("127.0.0.1", "9071", "/tmp/app", "/var/www/app")
	if err != nil {
		t.Fatalf("mcpSession: %v", err)
	}
	if s.dbgAddr != "127.0.0.1:9071" {
		t.Errorf("dbgAddr = %q, want 127.0.0.1:9071", s.dbgAddr)
	}
	if s.localRoot != "/tmp/app" || s.dockerRoot != "/var/www/app" {
		t.Errorf("roots = %q, %q, want /tmp/app, /var/www/app", s.localRoot, s.dockerRoot)
	}
}

func TestBadListenAddrFailsBeforeTheServerStarts(t *testing.T) {
	// RunE builds the session before serving, so an address the listener cannot
	// use is reported and nothing starts — on the mcp subcommand too, which
	// inherits the flag from the root.
	for _, args := range [][]string{
		{"--listen-addr", "localhost"},
		{"mcp", "--listen-addr", "1.2.3"},
	} {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), `invalid --listen-addr`) {
			t.Errorf("Execute(%q) = %v, want an invalid --listen-addr error", args, err)
		}
	}
}

func TestDbgListenAddr(t *testing.T) {
	tests := []struct {
		listenAddr string
		want       string
	}{
		{"0.0.0.0", "0.0.0.0:9003"},
		{"127.0.0.1", "127.0.0.1:9003"},
		{"172.17.0.1", "172.17.0.1:9003"},
		{"::1", "[::1]:9003"},
		{"::", "[::]:9003"},
	}
	for _, tt := range tests {
		got, err := dbgListenAddr(tt.listenAddr, "9003")
		if err != nil {
			t.Errorf("dbgListenAddr(%q) error: %v", tt.listenAddr, err)
			continue
		}
		if got != tt.want {
			t.Errorf("dbgListenAddr(%q) = %q, want %q", tt.listenAddr, got, tt.want)
		}
	}
}

func TestDbgListenAddrRejectsANonIP(t *testing.T) {
	// --listen-addr takes an IP literal only: a name such as localhost may
	// resolve to ::1 first, so the listener would bind an address the caller did
	// not write.
	for _, listenAddr := range []string{"localhost", "example.com", "1.2.3", "", "127.0.0.1:9003"} {
		got, err := dbgListenAddr(listenAddr, "9003")
		if err == nil {
			t.Errorf("dbgListenAddr(%q) = %q, want an error", listenAddr, got)
		}
	}
}

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name    string
		ldflags string
		bi      *debug.BuildInfo
		want    string
	}{
		{"ldflags wins over build info", "1.2.3", &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, "1.2.3"},
		{"dev ldflags falls back to build info", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, "1.2.3"},
		{"empty ldflags uses build info", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, "1.2.3"},
		{"nil build info reports dev", "", nil, "dev"},
		{"devel reports dev", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
		{"empty version reports dev", "", &debug.BuildInfo{Main: debug.Module{Version: ""}}, "dev"},
		{"pseudo-version reports dev", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260101120000-abcdef123456"}}, "dev"},
		{"pre-release pseudo-version reports dev", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.4-0.20260101120000-abcdef123456"}}, "dev"},
		{"pre-release tag is kept without the v prefix", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3-rc.1"}}, "1.2.3-rc.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.ldflags, tt.bi); got != tt.want {
				t.Errorf("resolveVersion(%q, %+v) = %q, want %q", tt.ldflags, tt.bi, got, tt.want)
			}
		})
	}
}

// TestResolvedVersionDefaultsToDev guards the local/dev build path: a test
// binary carries no release tag, so the resolved version must stay "dev".
func TestResolvedVersionDefaultsToDev(t *testing.T) {
	if got := resolvedVersion(); got != "dev" {
		t.Errorf("resolvedVersion() = %q, want dev", got)
	}
}
