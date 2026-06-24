# xdbg — docker-aware Xdebug debugger (MCP server)

A self-contained Go debugger that **is** the Xdebug client: it accepts the
container's DBGp connection and exposes `docker_xdebug_*` tools over MCP. It's a
superset of PhpStorm's Xdebug MCP tools, fixing their limitations:

- **Any HTTP method + headers + body** for `docker_xdebug_request` (PhpStorm's is GET-only).
- **CLI / Symfony command** debugging via `docker_xdebug_listen`.
- **Host↔container path translation** — set breakpoints with host paths; stacks
  show host paths. No `socat`, no PhpStorm.

## How it works

The container's Xdebug is a DBGp *engine*: with `xdebug.start_with_request=yes`
it dials out to `host.docker.internal:9003` on every request and waits for
commands. `xdbg` listens there (IPv4 `0.0.0.0:9003`) and drives it. The MCP
server is one long-lived process, so it holds the session across tool calls.

## Build & register

```bash
make xdbg-build          # -> tools/xdbg/xdbg
```

Registered in repo-root `.mcp.json` as the stdio server `xdbg`; tools appear as
`mcp__xdbg__docker_xdebug_*`. After changing `.mcp.json` or rebuilding, reconnect
MCP in the client.

Config (flags in `.mcp.json`): `--dbg-port` (DBGp listen port, default 9003),
`--local-root`, `--docker-root` (for path translation).

Shell completion:

```bash
xdbg completion zsh > ~/.zsh/completions/_xdbg   # zsh
xdbg completion bash > /etc/bash_completion.d/xdbg  # bash
xdbg completion fish > ~/.config/fish/completions/xdbg.fish  # fish
```

## Prerequisite

Xdebug must be enabled in the container (it's off by default for perf):

```bash
docker compose exec php-sub-api xdebug 1   # or: make xdebug-on
# ... debug ...
docker compose exec php-sub-api xdebug 0   # restore (make xdebug-off)
```

Also free port 9003: don't run alongside `socat` or PhpStorm's IDE listener.

## Tools (`mcp__xdbg__docker_xdebug_*`)

| Tool | Args |
|---|---|
| `docker_xdebug_status` | — |
| `docker_xdebug_set_breakpoint` | `file` (HOST path, auto-translated), `line` |
| `docker_xdebug_breakpoint_list` / `_remove` | — / `id` |
| `docker_xdebug_request` | `url`, `method`?, `headers`?, `body`?, `timeoutMs`? |
| `docker_xdebug_listen` | `timeoutMs`? (wait for next CLI/command session) |
| `docker_xdebug_run` / `_step_into` / `_step_over` / `_step_out` / `_pause` | — |
| `docker_xdebug_stack` | — |
| `docker_xdebug_context` | `stackDepth`? |
| `docker_xdebug_eval` | `expression` |
| `docker_xdebug_property_get` / `_set` | `name`(,`stackDepth`) / `name`,`value` |
| `docker_xdebug_detach` / `_stop` | — |

## Typical flows

**Web (POST/GET/…)** — the tool fires the request itself:
1. `docker_xdebug_set_breakpoint` `{file:"src/.../FooController.php", line:42}`
2. `docker_xdebug_request` `{url:"http://127.0.0.1:8090/api/foo", method:"POST", headers:{"Content-Type":"application/json"}, body:"{...}"}` → breaks at FooController:42
3. `docker_xdebug_stack` / `_context` / `_eval` / `_step_*` / `_run`

**CLI / Symfony command:**
1. `docker_xdebug_set_breakpoint` …
2. `docker_xdebug_listen` (arms; returns when the engine connects)
3. launch separately: `docker compose exec -T php-sub-api php bin/console app:cmd`
4. drive with `_run` / `_step_*` / `_stack` / `_context` / `_eval`

## Standalone (no MCP client)

A curl control API for manual use:

```bash
make xdbg          # xdbg --mcp=false --http 127.0.0.1:9010
curl 'localhost:9010/bp?file=public/index.php&line=8'
curl 'localhost:9010/request?url=http://127.0.0.1:8090/&method=POST&body={...}'
curl localhost:9010/stack ; curl 'localhost:9010/eval?expr=$x' ; curl localhost:9010/run
```

## Files

`main.go` (cobra CLI/wire-up), `session.go` (listener, adopt, commands, path xlat),
`dbgp.go` (wire framing + XML/base64 decode), `httpreq.go` (request firing),
`mcp.go` (JSON-RPC stdio), `httpctl.go` (standalone control API).

Note: DBGp XML declares `iso-8859-1`; parsed with a permissive `CharsetReader`
(values are ASCII/base64).
