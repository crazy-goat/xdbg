#!/usr/bin/env bash
# Run all static analysis, linters and formatter checks. --fix applies fixes first.
set -uo pipefail
cd "$(dirname "$0")/.."

FIX=0
[ "${1:-}" = "--fix" ] && FIX=1
failed=()

step() {
    local name="$1"; shift
    echo "==> $name"
    "$@" || failed+=("$name")
}

gofmt_check() {
    local out
    out=$(gofmt -l .) || return 1
    if [ -n "$out" ]; then
        echo "Not gofmt-formatted:" >&2
        echo "$out" >&2
        return 1
    fi
}

if [ "$FIX" = 1 ]; then
    step "gofmt -w" gofmt -w .
fi

step "gofmt" gofmt_check
step "go vet" go vet ./...
step "golangci-lint" golangci-lint run
step "shellcheck" bash -c 'git ls-files -z "*.sh" | xargs -0 -r shellcheck'

if [ "${#failed[@]}" -gt 0 ]; then
    echo "Failed: ${failed[*]}" >&2
    exit 1
fi
echo "All checks passed."
