#!/usr/bin/env bash
# Download Go module dependencies.
# Called by bin/worktree.sh after a new worktree is created.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
go mod download
