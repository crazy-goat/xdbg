// xdbg — a self-contained, docker-aware Xdebug (DBGp) debugger.
//
// Primary mode: an MCP stdio server exposing xdbg tools (full HTTP
// method/header/body control for requests, host<->container path translation,
// CLI/command debugging). Spawned by an MCP client (e.g. Claude Code) via
// .mcp.json.
//
//	xdbg mcp --dbg-port 9003 --local-root /Users/.../app --docker-root /var/www/app
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// version is the release version reported by `xdbg --version` and the MCP
// serverInfo. Release binaries inject it at build time with
// -ldflags "-X main.version=..."; any other build keeps the "dev" default.
var version = "dev"

// pseudoVersionRE matches a Go pseudo-version, e.g.
// v0.0.0-20260101120000-abcdef123456 or v1.2.4-0.20260101120000-abcdef123456.
// The 14-digit timestamp follows a '.' or '-' and precedes the commit hash.
// A pseudo-version is a commit, not a release, so xdbg reports "dev" for it.
var pseudoVersionRE = regexp.MustCompile(`[.-][0-9]{14}-[0-9a-f]+`)

// resolvedVersion is the version to report: the -ldflags value when set,
// otherwise the module build info (which `go install module@vX.Y.Z` fills with
// the tag), otherwise "dev". It reads the ldflags target on every call, so it
// stays correct if the variable is set after start (tests) or by the linker.
func resolvedVersion() string {
	bi, _ := debug.ReadBuildInfo()
	return resolveVersion(version, bi)
}

// resolveVersion picks the version to report. The -ldflags value wins when it
// is set to something other than the "dev" default. Otherwise it falls back to
// the main module build info, but only for a real release: an empty version,
// "(devel)" (local builds) and pseudo-versions (branch installs such as
// `go install ...@main`) all report "dev". A nil build info also reports "dev".
// Module versions carry a "v" prefix (`v1.2.3`) while release binaries inject
// the bare tag (`1.2.3`), so the prefix is stripped to report one string.
func resolveVersion(ldflags string, bi *debug.BuildInfo) string {
	if ldflags != "" && ldflags != "dev" {
		return ldflags
	}
	if bi == nil {
		return "dev"
	}
	v := bi.Main.Version
	if v == "" || v == "(devel)" || pseudoVersionRE.MatchString(v) {
		return "dev"
	}
	return strings.TrimPrefix(v, "v")
}

func getwdDefault() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime)
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		dbgPort          string
		listenAddr       string
		localRoot        string
		dockerRoot       string
		xdebugEnableCmd  string
		xdebugDisableCmd string
		xdebugStatusCmd  string
		containerExec    string
	)

	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run as MCP stdio server (default command)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := mcpSession(listenAddr, dbgPort, localRoot, dockerRoot)
			if err != nil {
				return err
			}
			s.enableCmd = xdebugEnableCmd
			s.disableCmd = xdebugDisableCmd
			s.statusCmd = xdebugStatusCmd
			s.containerExec = containerExec
			s.projectDir = localRoot
			log.Printf("MCP stdio server ready (xdbg tools)")
			newMCP(s).serve()
			return nil
		},
	}

	root := &cobra.Command{
		Use:   "xdbg",
		Short: "Docker-aware Xdebug (DBGp) debugger — MCP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return mcpCmd.RunE(mcpCmd, args)
		},
		SilenceUsage: true,
		Version:      resolvedVersion(),
	}
	root.AddCommand(mcpCmd)

	f := root.PersistentFlags()
	f.StringVar(&dbgPort, "dbg-port", "9003", "DBGp listen port (where container Xdebug connects)")
	f.StringVar(&listenAddr, "listen-addr", "0.0.0.0", "address the DBGp listener binds to, as an IP literal")
	f.StringVar(&localRoot, "local-root", getwdDefault(), "host project root (defaults to CWD)")
	f.StringVar(&dockerRoot, "docker-root", "", "container project root (default empty: no path translation; relative paths use local-root)")
	f.StringVar(&xdebugEnableCmd, "xdebug-enable-cmd", "", `shell command to enable Xdebug in the container, e.g. "docker compose exec -T php set-xdebug-on"`)
	f.StringVar(&xdebugDisableCmd, "xdebug-disable-cmd", "", `shell command to disable Xdebug in the container`)
	f.StringVar(&xdebugStatusCmd, "xdebug-status-cmd", "", `shell command to check Xdebug status in the container`)
	f.StringVar(&containerExec, "container-exec", "docker compose exec -T php", "prefix for running commands in the container")

	return root
}

// mcpSession builds the session the mcp command runs with. The listen address
// the flags carry becomes the session's bind address here, in one place a test
// can observe without starting the server.
func mcpSession(listenAddr, dbgPort, localRoot, dockerRoot string) (*session, error) {
	addr, err := dbgListenAddr(listenAddr, dbgPort)
	if err != nil {
		return nil, err
	}
	s := newSession(localRoot, dockerRoot)
	s.dbgAddr = addr
	return s, nil
}

// dbgListenAddr validates the --listen-addr value and joins it with the DBGp
// port. Only an IP literal is accepted: a name such as "localhost" resolves to
// ::1 before 127.0.0.1 on some hosts, so the listener would bind an address the
// caller did not write. JoinHostPort adds the brackets an IPv6 literal needs.
func dbgListenAddr(listenAddr, dbgPort string) (string, error) {
	if net.ParseIP(listenAddr) == nil {
		return "", fmt.Errorf("invalid --listen-addr %q: must be an IP address", listenAddr)
	}
	return net.JoinHostPort(listenAddr, dbgPort), nil
}
