#!/usr/bin/env sh
set -eu

root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
mode="${1:-check}"

generate() {
    package="$1"
    output="$2"
    temporary="$(mktemp)"
    trap 'rm -f "$temporary"' EXIT HUP INT TERM
    (cd "$root" && go doc -all "$package") | sed '${/^$/d;}' >"$temporary"

    if [ "$mode" = "--update" ]; then
        mkdir -p "$(dirname -- "$output")"
        mv "$temporary" "$output"
        trap - EXIT HUP INT TERM
        return
    fi

    diff -u "$output" "$temporary"
    rm -f "$temporary"
    trap - EXIT HUP INT TERM
}

generate github.com/faustbrian/golib/pkg/migrations "$root/api/migrations.txt"
generate github.com/faustbrian/golib/pkg/migrations/conformance "$root/api/conformance.txt"
generate github.com/faustbrian/golib/pkg/migrations/postgres "$root/api/postgres.txt"
