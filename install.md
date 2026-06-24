# Installation & Configuration

## Prerequisites

- **Go 1.21+** — required to build from source or via `go install`
- **Docker** (or compatible container runtime) running your PHP application
- **Xdebug 3.x** installed inside the PHP container, configured with `xdebug.start_with_request=yes` and `xdebug.client_host=host.docker.internal`

## Install

### Option A — `go install` (recommended)

```bash
go install github.com/crazy-goat/xdbg@latest
```

The binary is placed in `$(go env GOPATH)/bin` (default `~/go/bin`). Add it to your `PATH`:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

Add the line above to your shell profile (`~/.zshrc`, `~/.bashrc`, etc.) for persistence.

Verify the installation:

```bash
xdbg --help
```

### Option B — Build from source

```bash
git clone https://github.com/crazy-goat/xdbg.git
cd xdbg
make install
```

This compiles the binary and copies it to `~/.local/bin/xdbg`. Ensure `~/.local/bin` is on your `PATH`.

To build without installing:

```bash
make build
```

This produces the `./xdbg` binary in the project root.

## Configure Xdebug in the container

Add the following to your PHP container's Xdebug configuration (e.g. `xdebug.ini` or `php.ini`):

```ini
[xdebug]
zend_extension=xdebug
xdebug.mode=debug
xdebug.start_with_request=yes
xdebug.client_host=host.docker.internal
xdebug.client_port=9003
```

> **Note:** Xdebug should be disabled by default for performance. Use the
> `--xdebug-enable-cmd` / `--xdebug-disable-cmd` flags (see below) to let the
> AI agent toggle it on demand.

## Configure the MCP client

xdbg runs as an MCP **stdio** server — the AI agent spawns it as a child
process and communicates via JSON-RPC over stdin/stdout. Register it once in
your agent's configuration.

### opencode

Add an entry under `mcp` in `~/.config/opencode/opencode.json` (or your
project-level `opencode.json`):

```jsonc
{
  "mcp": {
    "xdbg": {
      "enabled": true,
      "type": "local",
      "command": [
        "xdbg",
        "--dbg-port", "9003",
        "--local-root",  "/absolute/path/to/your/project/on/host",
        "--docker-root", "/var/www/your-project",
        "--xdebug-enable-cmd",  "docker compose exec -T php set-xdebug-on",
        "--xdebug-disable-cmd", "docker compose exec -T php set-xdebug-off",
        "--xdebug-status-cmd",  "docker compose exec -T php get-xdebug-status",
        "--container-exec",      "docker compose exec -T php"
      ]
    }
  }
}
```

Restart opencode or reconnect MCP. Tools appear as `xdbg_*`.

### Claude Code

Create or edit `.mcp.json` in your project root (or `~/.claude.json` for global):

```json
{
  "mcpServers": {
    "xdbg": {
      "command": "xdbg",
      "args": [
        "--dbg-port", "9003",
        "--local-root",  "/absolute/path/to/your/project/on/host",
        "--docker-root", "/var/www/your-project",
        "--xdebug-enable-cmd",  "docker compose exec -T php set-xdebug-on",
        "--xdebug-disable-cmd", "docker compose exec -T php set-xdebug-off",
        "--xdebug-status-cmd",  "docker compose exec -T php get-xdebug-status",
        "--container-exec",      "docker compose exec -T php"
      ]
    }
  }
}
```

Reconnect MCP in Claude Code. Tools appear as `mcp__xdbg__xdbg_*`.

### Cursor / other MCP clients

Use the same `command` + `args` structure as the Claude Code example above,
adapted to your client's MCP configuration format.

## CLI flags reference

| Flag | Default | Description |
|---|---|---|
| `--dbg-port` | `9003` | Port Xdebug connects to (the listener binds `0.0.0.0:<port>`) |
| `--local-root` | — | Host project root — used for host-to-container path translation |
| `--docker-root` | — | Container project root — used for container-to-host path translation |
| `--xdebug-enable-cmd` | — | Shell command to enable Xdebug in the container |
| `--xdebug-disable-cmd` | — | Shell command to disable Xdebug in the container |
| `--xdebug-status-cmd` | — | Shell command to check Xdebug status in the container |
| `--container-exec` | `docker compose exec -T php` | Prefix for running CLI commands inside the container |


## Verify the setup

1. Ensure Xdebug is enabled in the container:
   ```bash
   docker compose exec -T php php -i | grep xdebug.mode
   # Should output: debug
   ```

2. Confirm the MCP server starts correctly:
   ```bash
   xdbg --dbg-port 9003 \
        --local-root /path/to/project \
        --docker-root /var/www/project
   # Should print: MCP stdio server ready (xdbg_*)
   ```

3. In your AI agent, verify tools are available. The agent should have access
   to tools prefixed with `xdbg_` (opencode) or `mcp__xdbg__xdbg_*` (Claude
   Code).

## Troubleshooting

| Problem | Solution |
|---|---|
| `xdbg: command not found` | Ensure `$(go env GOPATH)/bin` or `~/.local/bin` is on your `PATH` |
| Port 9003 already in use | Another debugger or xdbg instance is holding it. Kill it or use `--dbg-port` with a different port |
| Xdebug doesn't connect | Verify `xdebug.client_host=host.docker.internal` in the container and that port 9003 is not blocked by a firewall |
| Tools don't appear in agent | Reconnect/restart the MCP client. Check that the `command` path resolves correctly |
