# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed
- Malformed `tools/call` params now return a JSON-RPC `-32602` error instead of calling a tool named `""`; a failed write of a response to stdout is logged to stderr; a malformed DBGp init packet drops the connection and a malformed DBGp response is returned as an error instead of an empty response. The four `//nolint:errcheck` comments are gone (#2)

### Added
- `bin/lint.sh` runs gofmt, `go vet`, golangci-lint and shellcheck; `--fix` applies gofmt first. CI has a single `lint` job that runs it; `dbgp.go` is now gofmt-formatted (#11)
- `docs/workflow.md` and `docs/release-workflow.md` follow the shared crazy-goat templates; project commands live in the new `AGENTS.md` (#1)
- `bin/` has the shared issue and worktree helper scripts, plus `worktree-setup.sh` and `worktree-teardown.sh` (#1)
- CI: `go vet`, `golangci-lint`, `go test -race` and `go build` with the aggregate `ci-ok` check; heavy jobs are skipped for documentation-only changes (#1)
- Release workflow: pushing a `vX.Y.Z` tag builds `xdbg` for linux and darwin (amd64, arm64) and publishes a GitHub release with notes from this file (#1)
- Dependabot for Go modules and GitHub Actions (#1)
- Issue forms and a pull request template (#1)

### Changed
- The example Docker Compose stack publishes its host port through `XDBG_EXAMPLE_PORT` (default `8888`) (#1)
