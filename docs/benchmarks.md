# Benchmark baselines

Native Go benchmarks cover the bounded, database-independent work performed by
the migration runtime:

- canonical migration parsing at 1 KiB and 1 MiB;
- filesystem source loading at 100 and 1,000 migrations;
- planning and status construction at 100 and 10,000 migrations, with 75% of
  the history applied; and
- PostgreSQL schema fingerprinting at 100 and 10,000 unsorted catalog objects.

Run the complete benchmark suite with memory statistics:

```sh
go test -run='^$' -bench=. -benchmem -count=5 ./...
```

Record the full output, Go version, operating system, architecture, and CPU with
every baseline. Compare a candidate against the same machine and toolchain with
`benchstat`; absolute timings from different hosts are not comparable:

```sh
go test -run='^$' -bench=. -benchmem -count=10 ./... > old.txt
# Check out or build the candidate revision.
go test -run='^$' -bench=. -benchmem -count=10 ./... > new.txt
benchstat old.txt new.txt
```

CI runs every benchmark once with `-benchtime=1x`. This smoke gate proves the
fixtures remain valid and the benchmark paths compile and execute. It does not
enforce absolute latency or allocation thresholds because shared CI runner noise
would make those thresholds unreliable. Performance changes should instead be
reviewed with statistically significant `benchstat` output from a stable host.

## Reference baseline

The following values are the median of three runs on 2026-07-15 from the initial
hardening baseline. The host used Go 1.26.5 on macOS 27.0, arm64, with an Apple
M4 Max CPU. They provide a review reference, not a cross-host performance
contract.

| Benchmark | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `PlanUp/migrations_100` | 9,862 | 32,768 | 8 |
| `PlanUp/migrations_10000` | 1,148,016 | 4,064,575 | 68 |
| `BuildStatus/migrations_100` | 12,835 | 39,808 | 8 |
| `BuildStatus/migrations_10000` | 1,445,431 | 4,769,086 | 68 |
| `ParseMigrationFile/bytes_1024` | 1,188 | 3,518 | 7 |
| `ParseMigrationFile/bytes_1048576` | 606,257 | 3,163,486 | 8 |
| `FSSourceLoad/migrations_100` | 433,716 | 1,811,794 | 2,122 |
| `FSSourceLoad/migrations_1000` | 3,556,108 | 18,113,669 | 21,934 |
| `Fingerprint/objects_100` | 21,146 | 110,265 | 120 |
| `Fingerprint/objects_10000` | 2,452,560 | 13,097,008 | 10,038 |
