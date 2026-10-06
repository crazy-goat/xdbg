package main

import (
	"bytes"
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
