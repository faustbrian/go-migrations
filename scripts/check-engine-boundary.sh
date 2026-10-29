#!/bin/sh
set -eu

root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$root"

baseline=api/v5.txt
if [ ! -s "$baseline" ]; then
    echo "Public API snapshot is missing: $baseline" >&2
    exit 1
fi
if grep -Ein 'goose|database/sql|\*sql\.|lib/pq|pgx/v5/stdlib' "$baseline"; then
    echo "A database/sql or retired engine identity escaped into the public API snapshot" >&2
    exit 1
fi

# pgx is the only database driver. database/sql, its driver adapters and the
# retired Goose engine must not return through production code or tests. The
# non-releasable v4/v5 interop harness drives published v4, which still takes
# a *sql.DB, so it is the one excluded directory (testdata/interop).
forbidden_imports="$(
    grep -RInE --include='*.go' --exclude-dir=interop \
        '"(database/sql|database/sql/driver|github\.com/lib/pq|github\.com/jackc/pgx/v5/stdlib|github\.com/pressly/goose[^"]*|github\.com/DATA-DOG/go-sqlmock)"' \
        . || true
)"
if [ -n "$forbidden_imports" ]; then
    echo "Only pgx may reach PostgreSQL; forbidden imports:" >&2
    printf '%s\n' "$forbidden_imports" >&2
    exit 1
fi

if grep -En 'github\.com/(pressly/goose|lib/pq|DATA-DOG/go-sqlmock|go-sql-driver/|mattn/go-sqlite3|jackc/pgx/v4)' go.mod; then
    echo "go.mod requires a retired engine or a non-pgx database driver" >&2
    exit 1
fi

if grep -En "VALUES .*'goose'" postgres/*.go; then
    echo "Goose identity escaped into the owned ledger" >&2
    exit 1
fi
