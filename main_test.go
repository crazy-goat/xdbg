package main

import (
	"bytes"
	"runtime/debug"
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
