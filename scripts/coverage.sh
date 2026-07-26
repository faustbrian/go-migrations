#!/usr/bin/env sh
set -eu

profile="${1:-coverage.out}"
packages="$(go list ./... | grep -Ev '/(conformance|examples)(/|$)')"

go test -covermode=atomic -coverprofile="$profile" $packages
coverage="$(go tool cover -func="$profile" | awk '/^total:/ {print $3}')"

if [ "$coverage" != "100.0%" ]; then
    echo "coverage is $coverage; want 100.0%" >&2
    exit 1
fi

echo "coverage: $coverage"
