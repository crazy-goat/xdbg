---
name: xdebug-docker-debug
description: |
  Use when the user wants to debug PHP code running in Docker with Xdebug via xdbg.
  Covers three debugging flows: HTTP requests, CLI commands (agent-driven), and
  CLI commands (manual launch). Includes error recovery and decision guidance.
---

# Xdebug Docker Debugging — Agent Guide

This project uses **xdbg** (an MCP server) to debug PHP inside Docker. You
have access to xdbg tools via MCP. Do not ask the user to run Docker commands
manually unless the MCP tools fail — the agent can drive the entire session.

## Which flow to use? — Decision Tree

| User wants to debug... | Pick flow |
|---|---|
| An API endpoint, controller action, or web page hit by HTTP | **Flow 1: HTTP Request** |
| A Symfony console command, worker, or cron job | **Flow 2: CLI via `run_command`** (preferred) |
| A command that `run_command` can't reach (e.g. different container, special entrypoint) | **Flow 3: CLI via `listen` + manual launch** |

## Preconditions (all flows)

Always start with these checks. If container toggling is not configured, skip steps 1–2.

1. **`container_status`** — is Xdebug enabled in the container?
2. If off: **`container_enable`**
3. **`status`** — is there a stale session from a previous debug run?
4. If stale: **`detach`** or **`stop`** to free port 9003.

> **Why this matters:** Xdebug in the container must have `xdebug.mode=debug` and `xdebug.start_with_request=yes` so the engine dials out to port 9003. If Xdebug is off, the request/command runs to completion with no debug connection.

---

## Flow 1: Debugging an HTTP Request (POST/GET/PUT/PATCH/DELETE)

Best for: API endpoints, controllers, middleware, event subscribers hit via HTTP.

### Example — POST endpoint with auth header

User says: *"The `/api/orders` endpoint returns 500 when I send a JSON payload. I suspect `OrderController::create` around line 42."*

**Agent steps:**

1. `container_status`
2. `container_enable` (if needed)
3. `set_breakpoint` `{file: "src/Controller/OrderController.php", line: 42}`
4. `request`:
   ```json
   {
     "url": "http://127.0.0.1:8090/api/orders",
     "method": "POST",
     "headers": {"Content-Type": "application/json", "Authorization": "Bearer <token>"},
     "body": "{\"product_id\": 123, \"quantity\": 2}"
   }
   ```
5. The tool returns with `break` at `OrderController.php:42`.
6. `stack` → see the call stack
7. `context` → inspect `$request`, `$orderService`, local variables
8. `eval('$request->getContent()')` → confirm what actually arrived
9. `step_over` → advance one line without descending into callees
10. `run` → continue to next breakpoint or end
11. `detach` or `stop` → end session
12. `container_disable` → turn Xdebug off (optional, restores performance)

### When the breakpoint is NOT hit

If `request` returns `(script finished)` without breaking:

- **Wrong line number** — the code at that line may not be executable (blank line, comment, brace). Try `set_breakpoint` with a different line, or `breakpoint_list` to confirm.
- **Wrong file path** — `set_breakpoint` uses host paths. Ensure it matches the project root. Try an absolute path if relative fails.
- **Xdebug off** — `container_status` shows off. Enable it.
- **Route doesn't reach this code** — the URL may hit a different controller or a cached response. Verify the route mapping.

### Handling secrets (JWT, cookies, API keys)

Never paste tokens into tool arguments. Use `request_from_files`:

1. Save headers to `/tmp/headers.txt`:
   ```
   Content-Type: application/json
   Authorization: Bearer eyJhbG...
   ```
2. Save body to `/tmp/body.json`:
   ```json
   {"product_id": 123}
   ```
3. `request_from_files`:
   ```json
   {"url": "http://127.0.0.1:8090/api/orders", "method": "POST", "headers_file": "/tmp/headers.txt", "body_file": "/tmp/body.json"}
   ```

---

## Flow 2: Debugging a CLI Command (agent-driven — `run_command`)

Best for: Symfony console commands, artisan commands, or any CLI entrypoint reachable via `docker compose exec -T php <command>`.

### Example — Symfony console command

User says: *"`bin/console app:process-queue --queue=orders` fails silently. I think `QueueProcessor::process` at line 87 throws an exception that's swallowed."*

**Agent steps:**

1. `container_status`
2. `container_enable` (if needed)
3. `set_breakpoint` `{file: "src/Service/QueueProcessor.php", line: 87}`
4. `run_command`:
   ```json
   {"command": "bin/console app:process-queue --queue=orders"}
   ```
5. The tool returns with `break` at `QueueProcessor.php:87`.
6. `context` → inspect `$queue`, `$orders`
7. `eval('$this->logger->getLogs()')` → check what was logged before the breakpoint
8. `step_into` → descend into the function call on the next line
9. `step_over` → advance without descending
10. `run` → continue to next breakpoint or finish
11. `detach` → let the command finish and see its output, or `stop` → kill it
12. `container_disable`

### Why `run_command` over `listen`

- **One tool call** — you don't need to coordinate with the user to launch the command separately.
- **Output available** — if the command finishes without breakpoints, you see the full stdout/stderr.
- **Timeout handling** — `run_command` waits for the Xdebug connection, not forever.

### When `run_command` is NOT suitable

- The command must be run in a different container or via a different entrypoint (e.g. `docker compose run --rm worker` instead of `docker compose exec php`).
- The command is triggered by an external scheduler (Cron, Kubernetes Job) and the agent can't run it.
- The container exec prefix is misconfigured. → Use Flow 3.

---

## Flow 3: Debugging a CLI Command (manual launch — `listen`)

Best for: When `run_command` can't reach the command (different container, special entrypoint, external trigger).

### Example — Worker in a different container

User says: *"The `worker` container runs `php bin/console messenger:consume async`. I need to debug the handler."*

**Agent steps:**

1. `container_status`
2. `container_enable` (if needed)
3. `set_breakpoint` `{file: "src/MessageHandler/ProcessOrderHandler.php", line: 15}`
4. **`listen`** — this arms the listener and blocks until the engine connects:
   ```json
   {"timeoutMs": 60000}
   ```
   The tool returns: `listener armed (fire-and-forget); check status to see if a session was adopted`.
5. **Tell the user** to launch the command (or you launch it if you have access):
   ```bash
   docker compose exec -T worker php bin/console messenger:consume async --limit=1
   ```
6. **`status`** — check if a session was adopted. Once the engine connects, `status` shows `break` at `ProcessOrderHandler.php:15`.
7. `stack`, `context`, `eval` — inspect and step as usual
8. `detach` or `stop` — end the session
9. `container_disable`

### Critical: `listen` is fire-and-forget

`listen` does **not** return when the engine connects. It returns immediately with `listener armed`. You must poll `status` to detect when the session is adopted.

If `status` still shows `no session` after the user launched the command:
- The command ran to completion without hitting the breakpoint (wrong line, wrong file).
- Xdebug is off in the worker container (check `container_status`).
- The worker container can't reach `host.docker.internal:9003` (network/firewall issue).

### Timeout tip

Set `timeoutMs` generously for `listen` (e.g. 60000 or 120000). The user may need time to switch terminals and run the command. If `listen` times out, the listener closes and you must call `listen` again before the next launch.

---

## Common Failure Modes & Recovery

| Symptom | Likely cause | Fix |
|---|---|---|
| `request` returns `(script finished)` immediately | Breakpoint not hit | Wrong line, wrong file, Xdebug off, or route mismatch. Check `breakpoint_list` and `container_status`. |
| `run_command` returns `(script finished)` | Breakpoint not hit, or command has no Xdebug trigger | Same as above. Try `listen` + manual launch to verify the command actually runs the code. |
| `status` shows `no session` after `listen` | Engine never connected | Xdebug off, wrong container, network issue. Ask user to verify `php -i \| grep xdebug.mode` inside the container. |
| `request` / `run_command` error: "session already active" | Stale session from previous run | `detach` or `stop` first. Always check `status` before starting a new session. |
| `step_into` behaves like `step_over` | No function call on current line, or function is internal/native | Normal. Use `step_over` or `run` instead. |
| Variables show `object {3 children}` | Nested objects are collapsed | Use `property_get('$variable')` or `property_get('$variable->property')` to drill in. |
| `eval` returns an error | Expression throws an exception or uses undefined variable | Check `context` first to see what's in scope. Try simpler expressions. |
| Port 9003 busy | Another debugger (PhpStorm, xdbg instance) holds it | `lsof -i :9003` to find the process. Kill it, or use `--dbg-port` on a different port. |
| Path translation wrong | `--local-root` or `--docker-root` mismatch | Verify paths: host root should map to container root. Breakpoint file paths must be under `local-root`. |

---

## Conversation Patterns — What to Say and Do

| User says | You should |
|---|---|
| *"Debug this endpoint: POST /api/foo with body {...}"* | `container_status` → enable → `set_breakpoint` → `request` → inspect |
| *"This command fails: bin/console app:bar"* | `container_status` → enable → `set_breakpoint` → `run_command` → inspect |
| *"I need to debug the worker container"* | `container_status` → enable → `set_breakpoint` → `listen` → instruct user to launch command → `status` → inspect |
| *"What's in $user here?"* | `context` or `property_get('$user')` or `eval('$user->getRoles()')` |
| *"Step into this function"* | `step_into` |
| *"Let it run to the next breakpoint"* | `run` |
| *"I'm done debugging"* | `detach` or `stop` → `container_disable` (optional) |
| *"Why didn't it break?"* | Check `container_status`, `breakpoint_list`, verify file path and line number. Try `listen` + manual launch to test connectivity. |
| *"The port is busy"* | Ask user to close PhpStorm/browser debugger, or run `lsof -i :9003` and kill the process. |

---

## Ending Every Session

Always clean up to leave port 9003 free and container performance restored:

1. `detach` (let finish) or `stop` (kill) — frees the debug session and port
2. `container_disable` — turns Xdebug off (optional but recommended)
3. `status` — confirm `no session` and port is free

> **Note:** If you forget `detach`/`stop`, the next `request` or `run_command` will fail with "session already active". Always check `status` before starting a new session.
