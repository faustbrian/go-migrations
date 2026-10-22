# migrations

[![CI](https://github.com/faustbrian/go-migrations/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/faustbrian/go-migrations/actions/workflows/ci.yml)
[![CodeQL](https://img.shields.io/badge/CodeQL-required-blue)](https://github.com/faustbrian/go-migrations/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/badge/coverage-risk_selected-blue)](CONTRIBUTING.md#verification)
[![Mutation](https://img.shields.io/badge/mutation-optional_local%2Fselected_scheduled-blue)](CONTRIBUTING.md#verification)
[![Documentation](https://img.shields.io/badge/docs-checked_in_CI-blue)](docs/)
[![Go Reference](https://pkg.go.dev/badge/github.com/faustbrian/go-migrations/v4.svg)](https://pkg.go.dev/github.com/faustbrian/go-migrations/v4)
[![Release](https://img.shields.io/github/v/release/faustbrian/go-migrations?sort=semver)](https://github.com/faustbrian/go-migrations/releases)
[![Go](https://img.shields.io/badge/go-1.27.0-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

`migrations` is an engine-neutral database migration runtime with a
PostgreSQL backend. It owns migration identity, planning, status, locking,
checksums, baselines, recovery, and the `public.migrations` ledger.
Goose is an internal, pinned SQL execution detail and never appears in the
public API.

This source tree contains the `github.com/faustbrian/go-migrations/v4` module.
V3.0.0 introduces the `public.migrations` ledger name. Published v1 and v2
releases remain available under their original module paths.
Application adoption is a separate deployment boundary. The minimum
supported language version is Go 1.27.0; build with Go 1.27.3 or newer, which
fixes GO-2026-6809 in `crypto/x509`.

## Install

Install the v4 module with:

```sh
go get github.com/faustbrian/go-migrations/v4@v4.0.0
```

V3 consumers upgrade by replacing `/v3` with `/v4` in imports and moving
service composition to the single Service v3 adapter (see
[migration](docs/compatibility.md#v3-to-v4)). The ledger, checksums and
advisory lock are unchanged, so no database alignment is required. V2 consumers
upgrade by replacing `/v2` with `/v3` in migration imports, stopping older
runners, and manually aligning the database ledger before starting v3. V1
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

The v4 module uses these import paths.

| Import path | Use |
| --- | --- |
| `github.com/faustbrian/go-migrations/v4` | Define immutable migrations, load sources, plan, inspect status, apply, roll back, baseline, and recover. |
| `github.com/faustbrian/go-migrations/v4/postgres` | Persist the owned ledger and execute migrations under a PostgreSQL advisory lock. |
| `github.com/faustbrian/go-migrations/v4/adapters/service` | Adapt a caller-constructed runner to the standard one-shot Service v3 migrate command. |
| `github.com/faustbrian/go-migrations/v4/conformance` | Verify an alternative backend against the public engine contract in tests. |

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
finite cleanup budget of 30 seconds by default; `WithUnlockTimeout` can replace
the runner release budget.
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

PostgreSQL operation and SQL execution categories match through `errors.Is`
without becoming extra branches in the cause tree. SQL execution redacts each
non-context failure to `ErrExecutionFailed`, preserving joined cancellation
and independent failures without exposing driver diagnostics. A context match
alone does not prove caller interruption: check the caller's context and all
remaining causes before treating a failure as a clean cancellation.

Go's standard `database/sql` transaction finalization and cleanup calls cannot
accept a context once driver cleanup begins. Use maintained drivers with
finite server and network timeouts plus a supervised deployment-process hard
deadline, and inspect database and ledger state before retry after expiry.

The runner starts no goroutines and has no `Close` or `Shutdown` method. The
caller retains the source, observer, and `*sql.DB` and must close the database.
Concurrent operations are serialized by the backend's advisory lock. An
observer may be called concurrently when callers share a runner. Events have no
SQL field, but custom dependency errors can contain sensitive text, and traversing
PostgreSQL error causes exposes driver details. Observer panics are contained.
Observers are trusted synchronous callbacks and must perform bounded,
nonblocking work. Honor the supplied context when applicable, including the caller's own finite bound for
cleanup events whose context is detached from cancellation. The runner cannot
preempt a blocked callback; a callback can delay lock release and the operation's
return. Use a bounded, nonblocking application-owned handoff for slow exporters.

## Service migrate command

`adapters/service.New` adapts a caller-constructed `Runner` to the standard
one-shot `service` migrate role. The caller loads typed configuration, prepares
only migration dependencies, and explicitly selects the runner operation. The
adapter adds no migration policy, HTTP listener, management server, readiness
check, retries, or long-lived resource ownership.

The adapter uses `github.com/faustbrian/go-service/v3` types: `New(Options[C]{...})`
returns an adapter whose `Prepare` callback yields an `Execution`. It does not
translate migration SQL, ledger rows or checksums.

Migration-only components start before the task and stop in reverse order after
it finishes or fails. A missing runner fails during plan construction. Use a
dedicated deployment job and select `Runner.Up`, `Plan`, `Status`, `Down`, or
recovery behavior explicitly according to the reviewed operation.

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

- Independent migration owners can derive `backend.ForNamespace("outbox")`.
  The selected ledger is exactly `public.golib_migrations_outbox`; default
  runners still use `public.migrations`. All owners share the native lock.
- `ReadLedger` performs bounded read-only discovery without creating a ledger.
  Its snapshot is not execution ownership; revalidate under the native lock.
- `InstallFresh` installs a complete transactional history and optional
  package-owned preflight atomically. Existing history is not adopted/replayed.
- `AcquireCommand` retains one connection and migration ownership across
  independent histories. `Execute` commits each transactional batch, native
  ledger rows and command receipts together. Exact replay checks the original
  source/base/key and resumes only its durable prefix. Release ownership on
  every path and reconstruct it after an uncertain commit acknowledgement.
  See [independent migration owners](docs/operations.md#independent-migration-owners)
  for limits, recovery and adoption requirements.

- Loaded source is validated against complete ledger history under the advisory
  lock; source loading happens before lock acquisition. Keep artifacts immutable.
- Applied files cannot be changed, renamed, removed, or reordered.
- Transactional migrations update schema and ledger atomically.
- Explicit no-transaction migrations persist dirty state before executing SQL.
- Dirty outcomes require a checksum-bound operator recovery decision.
- Existing Laravel databases are adopted through a reviewed selected-catalog
  fingerprint after relocating Laravel history out of `public.migrations`.
  Grants/ownership, RLS flags/policy roles and other omitted properties need
  separate operator review.
- Status, plans, records and migration values protect their retained state.
  Events have no SQL field, but custom dependency errors may retain sensitive
  causes. A failed invocation can follow durable changes; inspect before retry.

Read the [migration format](docs/migration-format.md),
[operations guide](docs/operations.md), and
[Laravel baseline runbook](docs/laravel-baseline.md) before production use.

## Documentation

Start with the [documentation index](docs/README.md). It organizes migration
formats, PostgreSQL integration, production operations, Laravel adoption, and
maintainer references. The v4
[API reference](https://pkg.go.dev/github.com/faustbrian/go-migrations/v4),
[executable example](examples/job/main.go), backend
[testing helper](https://pkg.go.dev/github.com/faustbrian/go-migrations/v4/conformance),
[FAQ and troubleshooting](docs/faq.md), [performance baselines](docs/benchmarks.md),
[compatibility policy](COMPATIBILITY.md), [changelog](CHANGELOG.md),
[support guide](SUPPORT.md), and [private security-reporting process](SECURITY.md)
cover the remaining adoption paths.

For ecosystem-wide selection and ownership guidance, see the versioned
[Golib ecosystem index](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/README.md)
and its [Persistence and durability family](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/design-language.md#package-families-and-selection).

## Development

`make check` and `make ci` use the published tool pinned in `.golib.yaml` for
bounded verification with PostgreSQL fixtures, the `integration` tag and
deterministic operation-gate cancellation/session-ownership assertions. The
unchanged cheap engine-boundary guard remains required locally and in CI.
Base-free checks run fuzz seeds; changed-source CI selects active smoke.
`make check-full` retains all explicit full campaigns. Scheduled CI uses the
shared bounded route. See [contributor verification](CONTRIBUTING.md#verification)
for standalone tests, public installation, disposable-cache sessions and
explicit release rehearsal; local gate tests do not prove live PostgreSQL locks.

## License

`migrations` is open-source software licensed under the
[MIT License](LICENSE).

## Documentation navigation

[Complete documentation map](docs/README.md#complete-documentation-map) · [Current source package map](docs/package-map.md)

Use the [versioned ecosystem entry](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/README.md) for package
selection. The [frozen compatibility cohort](https://github.com/faustbrian/go-library-tools/blob/v1.8.4/docs/ecosystem/compatibility-sets.md)
is separate from current package releases and unpublished source changes.
Adopting them does not silently update that cohort.
