# migrations

[![CI](https://github.com/faustbrian/go-migrations/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/faustbrian/go-migrations/actions/workflows/ci.yml)
[![CodeQL](https://img.shields.io/badge/CodeQL-required-blue)](https://github.com/faustbrian/go-migrations/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/badge/coverage-100%25_required-blue)](CONTRIBUTING.md#verification)
[![Mutation](https://img.shields.io/badge/mutation-100%25_required-blue)](CONTRIBUTING.md#verification)
[![Documentation](https://img.shields.io/badge/docs-checked_in_CI-blue)](docs/)
[![Go Reference](https://pkg.go.dev/badge/github.com/faustbrian/go-migrations/v3.svg)](https://pkg.go.dev/github.com/faustbrian/go-migrations/v3)
[![Release](https://img.shields.io/github/v/release/faustbrian/go-migrations?sort=semver)](https://github.com/faustbrian/go-migrations/releases)
[![Go](https://img.shields.io/badge/go-1.27.0-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

`migrations` is an engine-neutral database migration runtime with a
PostgreSQL backend. It owns migration identity, planning, status, locking,
checksums, baselines, recovery, and the `public.migrations` ledger.
Goose is an internal, pinned SQL execution detail and never appears in the
public API.

This source tree contains the `github.com/faustbrian/go-migrations/v3` module.
V3.0.0 introduces the `public.migrations` ledger name. Published v1 and v2
releases remain available under their original module paths.
Application adoption is a separate deployment boundary. The minimum
supported and tested toolchain is Go 1.27.0.

## Install

Install v3.0.0 once published with:

```sh
go get github.com/faustbrian/go-migrations/v3@v3.0.0
```

Existing consumers can remain on published v1 or v2 releases. V2 consumers
upgrade by replacing `/v2` with `/v3` in migration imports and stopping older
runners and manually align the database ledger before starting v3. V1
consumers must also provide the bounded, cancellation-aware `SourceFileSystem` contract and review the
finite PostgreSQL timeout defaults. Laravel history must be relocated out of
`public.migrations` before adoption; see the baseline runbook.

The supported Go and PostgreSQL versions are documented in
[compatibility](docs/compatibility.md).

## Quick start

The complete [embedded-migrations job](examples/job/main.go) is the
compiler-built quick start. It opens a caller-owned `*sql.DB`, constructs the
filesystem source, PostgreSQL backend, and runner, inspects the plan, applies
pending migrations, and reads final status. From a checkout, point it at a
disposable PostgreSQL database:

```sh
DATABASE_URL='postgres://localhost:5432/example?sslmode=disable' \
  go run ./examples/job
```

The example migrations live beside the command in
[`examples/job/migrations`](examples/job/migrations). Run this code in a
dedicated deployment job; do not run it implicitly in every service process.

## Packages

The v3 module uses these import paths.

| Import path | Use |
| --- | --- |
| `github.com/faustbrian/go-migrations/v3` | Define immutable migrations, load sources, plan, inspect status, apply, roll back, baseline, and recover. |
| `github.com/faustbrian/go-migrations/v3/postgres` | Persist the owned ledger and execute migrations under a PostgreSQL advisory lock. |
| `github.com/faustbrian/go-migrations/v3/adapters/service` | Adapt a caller-constructed runner to the standard one-shot `service` migrate command. |
| `github.com/faustbrian/go-migrations/v3/migrationsservice` | Preserve the released service-adapter API while migrating imports to `adapters/service`. |
| `github.com/faustbrian/go-migrations/v3/conformance` | Verify an alternative backend against the public engine contract in tests. |

`examples/job` is an executable integration example, not a reusable package.

## When to use it

Use this module when an application needs immutable embedded SQL migrations,
an owned PostgreSQL ledger, deterministic plans, and explicit dirty-state
recovery. Use the conformance package when replacing the execution backend.
Do not use it as an ORM, schema declaration language, service-startup hook, or
wrapper around Laravel or Goose migration history. Deployment orchestration,
retry policy, and ownership of the application database remain outside this
module.

## Construction and lifecycle

`NewFSSource`, `postgres.New`, and `NewRunner` validate their inputs without
opening a migration session or executing SQL. `NewRunner` defaults lock-release
cleanup to 30 seconds. The PostgreSQL backend polls a held advisory lock every
100 milliseconds, defaults lock acquisition to 30 seconds, and defaults each
migration statement to five minutes. Primary work in each operation, including
waiting for serialized session ownership, defaults to ten minutes. Detached
best-effort statement-timeout restoration and lock release each use a separate
finite cleanup budget of at most 30 seconds.
`WithLockTimeout`, `WithStatementTimeout`, and `WithOperationTimeout` replace
those defaults with positive finite values; zero and negative overrides are
rejected. Invalid options and nil collaborators fail construction.

`NewFSSource` accepts a caller-owned `SourceFileSystem`. A complete source load
defaults to ten minutes; `WithSourceTimeout` accepts a positive finite
override. Its `ReadDir` and
`ReadFile` implementations must pass the supplied context to every blocking
operation and enforce the supplied count and byte limits before retaining or
returning data. The package revalidates returned inventories and file bytes.
The embedded example implements this boundary over compiler-owned immutable
data; networked or disk-backed providers need native cancellation-aware I/O.

Runner and PostgreSQL backend functional options are applied from left to
right. Construction stops at the first nil or failing option, so later options
do not run; repeated valid settings use the last applied value. Neither
constructor acquires a migration lock, runs SQL, or takes ownership of the
caller-provided database while applying options.

Every plan, status, apply, rollback, baseline, and recovery operation accepts
the caller's `context.Context`. It acquires one connection-bound session,
validates the complete source and ledger, and releases the session before
returning. Cancellation is returned through the ordinary error chain;
non-transactional work can instead leave a visible dirty record when the
external outcome is not safely reversible. Stable error categories support
`errors.Is`, while wrapped causes retain operational detail.

Go's standard `database/sql` transaction finalization and cleanup calls cannot
accept a context once driver cleanup begins. Use maintained drivers with
finite server and network timeouts plus a supervised deployment-process hard
deadline, and inspect database and ledger state before retry after expiry.

The runner starts no goroutines and has no `Close` or `Shutdown` method. The
caller retains the source, observer, and `*sql.DB` and must close the database.
Concurrent operations are serialized by the backend's advisory lock. An
observer may be called concurrently when callers share a runner and never
receives migration SQL; observer panics are contained. Observers are trusted
synchronous callbacks and must perform bounded, nonblocking work. Honor the
supplied context when applicable, including the caller's own finite bound for
cleanup events whose context is detached from cancellation. The runner cannot
preempt a blocked callback; a callback can delay lock release and the operation's
return. Use a bounded, nonblocking application-owned handoff for slow exporters.

## Service migrate command

`adapters/service.New` adapts a caller-constructed `Runner` to the standard
one-shot `service` migrate role. The caller loads typed configuration, prepares
only migration dependencies, and explicitly selects the runner operation. The
adapter adds no migration policy, HTTP listener, management server, readiness
check, retries, or long-lived resource ownership.

Migration-only components start before the task and stop in reverse order after
it finishes or fails. A missing runner fails during plan construction. Use a
dedicated deployment job and select `Runner.Up`, `Plan`, `Status`, `Down`, or
recovery behavior explicitly according to the reviewed operation.

The v3 `migrationsservice` path is a deprecated compatibility facade for the
v1 API. It preserves generic signatures, named type identities, error
sentinels and traversal, command semantics, and caller-owned runner and resource
lifecycle while delegating to `adapters/service`.

## Integrations and companion packages

- [`go-service`](https://github.com/faustbrian/go-service) supplies the
  one-shot command contract used by `adapters/service`.
- [`go-postgres`](https://github.com/faustbrian/go-postgres) can supply the
  caller-owned `*sql.DB`; the [integration guide](docs/go-postgres.md) keeps
  the two libraries as sibling dependencies.
- [`go-idempotency`](https://github.com/faustbrian/go-idempotency) and
  [`go-transactional-outbox`](https://github.com/faustbrian/go-transactional-outbox)
  compose with migrations in durable PostgreSQL services without becoming
  migration-runtime dependencies.

## Safety properties

- The complete source and ledger history is validated while an advisory lock is
  held.
- Applied files cannot be changed, renamed, removed, or reordered.
- Transactional migrations update schema and ledger atomically.
- Explicit no-transaction migrations persist dirty state before executing SQL.
- Dirty outcomes require a checksum-bound operator recovery decision.
- Existing Laravel databases are adopted through an exact reviewed schema
  fingerprint after relocating Laravel history out of `public.migrations`.
- Status, plans, records, events, and migration values are immutable snapshots.

Read the [migration format](docs/migration-format.md),
[operations guide](docs/operations.md), and
[Laravel baseline runbook](docs/laravel-baseline.md) before production use.

## Documentation

Start with the [documentation index](docs/README.md). It organizes migration
formats, PostgreSQL integration, production operations, Laravel adoption, and
maintainer references. The v3
[API reference](https://pkg.go.dev/github.com/faustbrian/go-migrations/v3),
[executable example](examples/job/main.go), backend
[testing helper](https://pkg.go.dev/github.com/faustbrian/go-migrations/v3/conformance),
[FAQ and troubleshooting](docs/faq.md), [performance baselines](docs/benchmarks.md),
[compatibility policy](COMPATIBILITY.md), [changelog](CHANGELOG.md),
[support guide](SUPPORT.md), and [private security-reporting process](SECURITY.md)
cover the remaining adoption paths.

For ecosystem-wide selection and ownership guidance, see the versioned
[Golib ecosystem index](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/README.md)
and its [Persistence and durability family](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/design-language.md#package-families-and-selection).

## License

`migrations` is open-source software licensed under the
[MIT License](LICENSE).
