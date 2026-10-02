#!/usr/bin/env bash
# Stop and remove the example Docker stack of this worktree (if it was started).
# Called by bin/worktree-done.sh before the worktree is removed.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if [[ -f .env.worktree ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env.worktree
  set +a
  export COMPOSE_ENV_FILES="$PWD/.env.worktree"
else
  unset COMPOSE_ENV_FILES
fi

if [[ -z "${COMPOSE_PROJECT_NAME:-}" ]]; then
  echo "COMPOSE_PROJECT_NAME is empty (no .env.worktree); refusing to stop the stacks of another checkout." >&2
  exit 1
fi

(cd example && docker compose down -v --remove-orphans) || true
