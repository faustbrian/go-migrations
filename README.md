# migrations

`migrations` is an engine-neutral database migration runtime with a
PostgreSQL backend. It owns migration identity, planning, status, locking,
checksums, baselines, recovery, and the `public.go_schema_migrations` ledger.
Goose is an internal, pinned SQL execution detail and never appears in the
public API.

## Install

```sh
go get github.com/faustbrian/golib/pkg/migrations
```

The supported Go and PostgreSQL versions are documented in
[compatibility](docs/compatibility.md).

## Embedded migrations

```go
//go:embed migrations/*.sql
var files embed.FS

source, err := migrations.NewFSSource(files, "migrations")
if err != nil {
	return err
}
backend, err := postgres.New(
	database,
	postgres.WithLockTimeout(30*time.Second),
	postgres.WithStatementTimeout(5*time.Minute),
)
if err != nil {
	return err
}
runner, err := migrations.NewRunner(source, backend)
if err != nil {
	return err
}
plan, err := runner.Plan(ctx)
if err != nil {
	return err
}
result, err := runner.Up(ctx)
```

Run this code in a dedicated deployment job. Do not run it implicitly in every
service process.

## Safety properties

- The complete source and ledger history is validated while an advisory lock is
  held.
- Applied files cannot be changed, renamed, removed, or reordered.
- Transactional migrations update schema and ledger atomically.
- Explicit no-transaction migrations persist dirty state before executing SQL.
- Dirty outcomes require a checksum-bound operator recovery decision.
- Existing Laravel databases are adopted through an exact reviewed schema
  fingerprint without reading or modifying Laravel's `migrations` table.
- Status, plans, records, events, and migration values are immutable snapshots.

Read the [migration format](docs/migration-format.md),
[operations guide](docs/operations.md), and
[Laravel baseline runbook](docs/laravel-baseline.md) before production use.

## Documentation

- [Architecture and engine contract](docs/architecture.md)
- [postgres integration](docs/postgres.md)
- [Migration format and ledger](docs/migration-format.md)
- [Operations and disaster recovery](docs/operations.md)
- [Laravel-to-Go baseline runbook](docs/laravel-baseline.md)
- [Security](docs/security.md)
- [Compatibility](docs/compatibility.md)
- [Hardening evidence](docs/hardening.md)
- [Benchmark baselines](docs/benchmarks.md)
- [Replacing the execution engine](docs/engine-replacement.md)
- [FAQ](docs/faq.md)
- [Contributing](CONTRIBUTING.md)

## License

`migrations` is open-source software licensed under the
[MIT License](LICENSE).
