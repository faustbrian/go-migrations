# Contributing

Use a conventional or Linear-style branch name and conventional commits. Add a
test before behavior changes. Run formatting, unit tests, vet, lint, race,
coverage, fuzz smoke tests, and the PostgreSQL integration matrix before a pull
request.

Performance-sensitive changes should include before-and-after results from the
[native benchmark suite](docs/benchmarks.md), captured on the same host and Go
toolchain.

Public changes must preserve the engine-neutral boundary: no Goose type, error,
file rule, or table may escape `internal`. Changes to migration identity, the
ledger, baseline fingerprinting, or recovery semantics require compatibility
analysis, migration guidance, fault tests, and changelog entries.

Integration tests require Docker. Run each supported version with:

```sh
for version in 14 15 16 17 18; do
  POSTGRES_VERSION=$version go test -tags=integration ./postgres \
    -run TestPostgresEngineConformance -count=1
done
```
