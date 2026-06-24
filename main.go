// xdbg — a self-contained, docker-aware Xdebug (DBGp) debugger.
//
// Primary mode: an MCP stdio server exposing docker_xdebug_* tools (full HTTP
// method/header/body control for requests, host<->container path translation,
// CLI/command debugging). Spawned by an MCP client (e.g. Claude Code) via
// .mcp.json.
//
//	xdbg --dbg-port 9003 --local-root /Users/.../subscription-api --docker-root /var/www/subscription-api
//
// Secondary mode (no MCP client): a curl-driven HTTP control API.
//
//	xdbg --mcp=false --http 127.0.0.1:9010
package main

import (
	"log"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime)

	var (
		dbgPort          string
		localRoot        string
		dockerRoot       string
		httpAddr         string
		mcpMode          bool
		xdebugEnableCmd  string
		xdebugDisableCmd string
		xdebugStatusCmd  string
		containerExec    string
	)

	root := &cobra.Command{
		Use:          "xdbg",
		Short:        "Docker-aware Xdebug (DBGp) debugger — MCP server + HTTP control API",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s := newSession(localRoot, dockerRoot)
			s.dbgAddr = "0.0.0.0:" + dbgPort
		s.enableCmd = xdebugEnableCmd
		s.disableCmd = xdebugDisableCmd
		s.statusCmd = xdebugStatusCmd
		s.containerExec = containerExec
		s.projectDir = localRoot
			if httpAddr != "" {
				go serveHTTP(s, httpAddr)
			}
			if mcpMode {
				log.Printf("MCP stdio server ready (docker_xdebug_*)")
				newMCP(s).serve()
				return nil
			}
			select {}
		},
	}

	f := root.Flags()
	f.StringVar(&dbgPort, "dbg-port", "9003", "DBGp listen port (where container Xdebug connects)")
	f.StringVar(&localRoot, "local-root", "/Users/piotr.halas/work/subscription-api", "host project root")
	f.StringVar(&dockerRoot, "docker-root", "/var/www/subscription-api", "container project root")
	f.StringVar(&httpAddr, "http", "", "HTTP control API address, e.g. 127.0.0.1:9010")
	f.BoolVar(&mcpMode, "mcp", true, "run as MCP stdio server (stdout = JSON-RPC channel)")
	f.StringVar(&xdebugEnableCmd, "xdebug-enable-cmd", "", `shell command to enable Xdebug in the container, e.g. "docker compose exec -T php xdebug 1"`)
	f.StringVar(&xdebugDisableCmd, "xdebug-disable-cmd", "", `shell command to disable Xdebug in the container`)
	f.StringVar(&xdebugStatusCmd, "xdebug-status-cmd", "", `shell command to check Xdebug status in the container`)
	f.StringVar(&containerExec, "container-exec", "docker compose exec -T php-sub-api", "prefix for running commands in the container (used by docker_xdebug_run_command)")

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
