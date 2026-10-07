# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed
- `run_command` now watches the container command and returns as soon as it exits when no Xdebug connection arrives, reporting the command output and exit status (or that Xdebug is off) instead of waiting for the full timeout and losing the output. A short grace still honours an engine that connects just before the process exits (#58)
- `xdbg --version` and the MCP `serverInfo.version` now fall back to the module build info when the `-ldflags` value is absent, so `go install github.com/crazy-goat/xdbg@vX.Y.Z` reports the tag instead of `dev`. Local builds and branch installs such as `@main` still report `dev`, and release binaries keep reporting the tag from `-ldflags` (#54)
- `listen`, `request`, `request_from_files` and `run_command` now keep waiting when Xdebug connects just before the accept timeout and the DBGp handshake finishes after it, instead of reporting "no engine connected" and leaving an orphan paused session. The handshake has its own deadline, and a timeout drops the accepted connection so the next `listen` works (#27)
- `xdbg` without the `mcp` subcommand accepts the same flags as `xdbg mcp` (#60)
- Session status now clears the last paused source location when Xdebug resumes, stops, detaches, or disconnects (#36)
- `summarize` now truncates long property values at 300 runes instead of 300 bytes and adds an ellipsis without splitting UTF-8 characters (#56)
- DBGp commands now quote and escape property names and encode file URIs. Breakpoints and property access support spaces, quotes, backslashes, and non-ASCII characters. Host locations decode file URIs, but plain paths retain literal percent sequences. Property names reject NUL bytes; breakpoint removal rejects non-numeric engine ids and retains local handles (#26)
- `breakpoint_remove` now accepts stable local handles (`q1`, `q2`, ...) to remove queued breakpoints without a session. Queued replies and queued or rejected list entries show the handles. Empty ids return an error without a state change. Failed engine removal leaves the local queue unchanged; `breakpoint_clear` remains best-effort (#29)
- `example/bin/set-xdebug-on` and `set-xdebug-off` now call the `xdebug-on` / `xdebug-off` scripts in the container and fail when the change fails (#59)
- `request_from_files` now accepts JSON headers files on one line or multiple lines. JSON files reject non-string values, including `null`, but accept empty strings. The line format rejects empty or invalid header names. Both request tools now honor `Host` headers, regardless of case. HTTP client errors return immediately instead of a misleading Xdebug timeout. The headers file descriptions now agree with the supported formats (#30)
- `breakpoint_list` now lists queued breakpoints as host paths without an active session and returns `(none)` for an empty list. With an active session, it lists engine breakpoints and queued entries without engine ids. Rejected entries retain their error text in both cases (#28)
- DBGp `<error>` responses now return command errors with the engine code and message instead of success. Engine errors do not close the session. A failed live breakpoint is not stored. Rejected queued breakpoints appear in stderr logs and `breakpoint_list`. `property_set` also returns an error for `success="0"` (#24)
- Path translation now respects directory boundaries and preserves `/` as a root. Root comparisons and suffix extraction use clean paths, so dot segments and repeated separators do not select the wrong root. An empty `--docker-root` resolves relative paths under the local root and leaves engine paths unchanged. When roots overlap, the more specific root takes precedence for breakpoint paths (#23)
- Invalid DBGp payload lengths no longer crash the MCP server or cause excessive allocation. xdbg rejects negative lengths and lengths above 64 MiB. A missing or invalid trailing NUL now causes an error and closes the engine connection (#22)

## [0.2.0] - 2026-10-06

### Added
- Unit tests for DBGp packet framing and XML parsing (`dbgp.go`), property decoding/summarizing, and the `--local-root` / `--docker-root` path translation (`session.go`), including malformed and truncated input (#3)
- `xdbg --version` prints the release version, and the MCP `serverInfo` reports the same value; `release.yaml` injects it from the tag with `-X main.version` (local builds report `dev`) (#4)
- `install.md` documents installing from the prebuilt release binaries (linux/darwin, amd64/arm64) and verifying them with `checksums.txt` (#5)
- `bin/lint.sh` runs gofmt, `go vet`, golangci-lint and shellcheck; `--fix` applies gofmt first. CI has a single `lint` job that runs it; `dbgp.go` is now gofmt-formatted (#11)
- `docs/workflow.md` and `docs/release-workflow.md` follow the shared crazy-goat templates; project commands live in the new `AGENTS.md` (#1)
- `bin/` has the shared issue and worktree helper scripts, plus `worktree-setup.sh` and `worktree-teardown.sh` (#1)
- CI: `go vet`, `golangci-lint`, `go test -race` and `go build` with the aggregate `ci-ok` check; heavy jobs are skipped for documentation-only changes (#1)
- Release workflow: pushing a `vX.Y.Z` tag builds `xdbg` for linux and darwin (amd64, arm64) and publishes a GitHub release with notes from this file (#1)
- Dependabot for Go modules and GitHub Actions (#1)
- Issue forms and a pull request template (#1)

### Changed
- The example Docker Compose stack publishes its host port through `XDBG_EXAMPLE_PORT` (default `8888`) (#1)
- The MCP server reports itself as `xdbg` instead of `docker-xdebug` in `serverInfo.name` (#4)

### Fixed
- A JSON message without a `method` (for example `{}` or `null`) now gets a `-32600` invalid request error instead of being taken for a notification and dropped silently (#18)
- The MCP server answers a line that is not valid JSON with a `-32700` parse error (or `-32600` for valid JSON that is not a request) and a null id, and logs it, instead of dropping it silently and leaving the client waiting (#15)
- A failed DBGp handshake (unreadable or malformed init packet) now wakes `request`, `listen` and `run_command` at once with a "handshake failed" error, instead of letting them wait for the full timeout and report a misleading "is Xdebug enabled?" message (#14)
- Malformed `tools/call` params now return a JSON-RPC `-32602` error instead of calling a tool named `""`; a failed write of a response to stdout is logged to stderr; a malformed DBGp init packet drops the connection and a malformed DBGp response is returned as an error instead of an empty response. The four `//nolint:errcheck` comments are gone (#2)
