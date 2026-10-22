#!/bin/sh
set -eu

root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$root"

baseline=api/v4.txt
if [ ! -s "$baseline" ]; then
    echo "Public API snapshot is missing: $baseline" >&2
    exit 1
fi
if grep -Ein 'goose' "$baseline"; then
    echo "Goose identity escaped into a public API snapshot" >&2
    exit 1
fi

direct_imports="$(
    grep -RIl --include='*.go' 'github.com/pressly/goose' . \
        | grep -v '^./internal/goose/' \
        || true
)"
if [ -n "$direct_imports" ]; then
    echo "Goose imports must remain under internal/goose:" >&2
    printf '%s\n' "$direct_imports" >&2
    exit 1
fi

if grep -En "VALUES .*'goose'" postgres/*.go; then
    echo "Goose identity escaped into the owned ledger" >&2
    exit 1
fi
