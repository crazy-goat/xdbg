# AGENTS.md

Project commands and specifics for xdbg. The development process (issue,
worktree, review, PR, merge) is in [docs/workflow.md](docs/workflow.md), the release
process in [docs/release-workflow.md](docs/release-workflow.md).

Everything is written in English (code, comments, docs, commits, issues).

xdbg is a self-contained, Docker-aware Xdebug (DBGp) debugger that runs as an MCP
stdio server. It is a single Go module, `github.com/crazy-goat/xdbg`, with one
`main` package at the repository root.

## Layout

| Path | Content |
|---|---|
| `main.go` | CLI entry point (cobra): flags and the `mcp` command |
| `mcp.go` | MCP JSON-RPC server over stdio and the tool definitions |
| `session.go` | DBGp session: listener, commands, breakpoints, path translation, container commands |
| `dbgp.go` | DBGp XML response structs and decoding helpers |
| `httpreq.go` | HTTP request sender used by the `request` tools |
| `example/` | Demo PHP project with Docker Compose (nginx + PHP with Xdebug) |
| `skills/xdbg/` | Agent skill that explains how to use the tools |
| `docs/` | Architecture diagrams and the example preview; process docs |
| `install.md` | Installation guide (also read by agents) |

Read `README.md` before changing tool names, flags or behaviour. They are the public interface.

## Commands

Go version: from `go.mod` (CI uses `go-version-file: go.mod`).

```bash
go build ./...                 # compile everything
go vet ./...
golangci-lint run ./...        # config: .golangci.yml (golangci-lint v2)
go test -race -count=1 ./...   # there are no test files yet

make build                     # builds to ~/.local/bin/xdbg
make install                   # same, see install.md
```

Run the server by hand (it speaks MCP on stdin/stdout, logs go to stderr):

```bash
go run . --dbg-port 9003 --local-root "$PWD/example" --docker-root /var/www/html
```

Normally an MCP client starts it, see `README.md`.

Example stack (needs Docker):

```bash
cd example && docker compose up -d --build    # http://localhost:8888
```

The host port is `XDBG_EXAMPLE_PORT` (default `8888`). `bin/worktree.sh` writes a free
port to `.env.worktree`; load it with `set -a && . ./.env.worktree && set +a`.
`bin/worktree-teardown.sh` stops the stack of the worktree (it refuses to run without
a `COMPOSE_PROJECT_NAME`).

The built binary (`/xdbg`) and `dist/` are generated and must never be committed.

## CI

`.github/workflows/tests.yaml` runs on pull requests and pushes to `main`. The `changes`
job detects documentation-only changes; the `docs` job checks them fast. `go vet`,
`golangci-lint`, `go test -race` and `go build` run only for code changes. The required
check is `ci-ok`. Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yaml`.

## Conventions

- Commit scopes: `mcp`, `session`, `dbgp`, `http`, `example`, `skills`, `docs`, `ci`.
  Example: `fix(session): close the listener on detach (#12)`.
- The tool names, flags and defaults in `README.md` are a public interface. Change them
  deliberately, keep them backward compatible where possible, and document the change
  in `CHANGELOG.md` and `README.md`.
- Logs go to stderr only. Stdout belongs to the MCP protocol; never print to it.
- `errcheck` is disabled in `.golangci.yml` because best-effort cleanup calls ignore
  errors on purpose. Handle errors that change behaviour.
- New code gets tests where it can be tested without a running PHP container (DBGp
  parsing, path translation, JSON-RPC handling).
- Milestone numbers are not versions. Use the milestone title (`vX.Y.Z`).
