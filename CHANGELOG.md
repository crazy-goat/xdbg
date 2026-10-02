# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `docs/workflow.md` and `docs/release-workflow.md` follow the shared crazy-goat templates; project commands live in the new `AGENTS.md`
- `bin/` has the shared issue and worktree helper scripts, plus `worktree-setup.sh` and `worktree-teardown.sh`
- CI: `go vet`, `golangci-lint`, `go test -race` and `go build` with the aggregate `ci-ok` check; heavy jobs are skipped for documentation-only changes
- Release workflow: pushing a `vX.Y.Z` tag builds `xdbg` for linux and darwin (amd64, arm64) and publishes a GitHub release with notes from this file
- Dependabot for Go modules and GitHub Actions
- Issue forms and a pull request template

### Changed
- The example Docker Compose stack publishes its host port through `XDBG_EXAMPLE_PORT` (default `8888`)
