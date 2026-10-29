# Compatibility

[Documentation index](README.md) · [Repository entry](../README.md)

This source contains the v5 line at `github.com/faustbrian/go-migrations/v5`.
Published releases keep their original module paths. The latest published v1,
v2, v3 and v4 releases remain supported; v5 becomes supported once published.
The module follows semantic versioning, and root releases
use `v<version>` tags. Compatible defect and security fixes may be backported to
a supported major; incompatible behavior requires a new major version and
migration guidance in `CHANGELOG.md`.

The source format and PostgreSQL schema fingerprint are explicitly versioned as
v1 contracts. Ledger history must remain readable across compatible releases.
Supported PostgreSQL major versions are 14, 15, 16, 17, and 18. Current hosted
CI exercises PostgreSQL 18; it does not establish a complete supported-version
matrix. PostgreSQL majors are removed only after upstream ends support and
the removal is documented. The Go version is declared by `go.mod`.

The PostgreSQL backend is driven by pgx v5 through a caller-owned
`*pgxpool.Pool`; `go.mod` selects the pgx version. Releases before v5 executed
migration SQL through an internal Goose adapter, so the compatibility corpus
still records which adapter version produced each historical fixture, while
persisted ledger rows contain only the owned PostgreSQL contract identity.

The published v1 line remains available at its original module path. The v2
line preserves persisted row formats but intentionally replaces plain `fs.FS`
source access with the cancellation-aware, bounded `SourceFileSystem` contract
and changes omitted PostgreSQL timeout options from unbounded behavior to finite
defaults. Consumers upgrade by adding `/v2` to migrations
imports, providing that source boundary, and reviewing the 30-second lock and
five-minute statement budgets.
`testdata/compatibility/v1` preserves v1 source identities, checksums, and
persisted row formats, with the table name manually aligned to the v3 ledger.
The original unaligned fixture remains available in published v1 and v2 tags.
Add a new fixture before changing the source format, checksum, or row contract.
The real PostgreSQL integration scenario installs the aligned schema and
historical row, then proves the current package can plan and append without
rewriting history.

Current unit and historical-ledger tests run against the pgx version selected
by `go.mod`; they do not execute a multi-version driver matrix. Historical
fixture producer versions remain recorded in the compatibility corpus.
Updating the pgx pin requires a changelog entry and a green persisted-ledger
scenario before release.

The ledger is now only `public.migrations`. Stop old runners and manually
align existing Go history before starting v3; no other table is recognized or
excluded from schema fingerprints. Resolve any conflicting ownership first.
The aligned v1 fixture proves row compatibility, not automatic table adoption.

V3 consumers must replace `/v2` with `/v3` in Go imports. The Go API is
otherwise unchanged from v2; the ledger rename and Laravel relocation
requirement are the breaking persisted-contract changes.

## v3 to v4

Version 4 removes the `migrationsservice` package, the Service v1 adapter
binding and the `NewV2`, `OptionsV2`, `ExecutionV2` and `AdapterV2` names.
`adapters/service` now has one binding on `github.com/faustbrian/go-service/v3`:
use `New`, `Options` and `Execution`, which take the native Service v3 types.
Replace `/v3` with `/v4` in imports and move the migrate command's service
composition to Service v3 in the same change. The binding was removed so
migration commands share the one Service lifecycle the rest of the fleet uses
instead of maintaining two adapter generations. The adapter adds no behavior
of its own: command name, one-shot ordering, cleanup and error classification
are unchanged. For a definition that registers only the migrate command, exit
codes for ordinary invocations are also unchanged. Parent cancellation is a
clean exit only when cleanup succeeds: a failed shutdown, shutdown timeout or
startup rollback that coincides with cancellation now exits 70, 124 or 75
instead of 0. Because no command in a migrate-only definition declares
options, Service v3 parses it with the canonical go-cli v2 grammar:
`migrate --` now runs the migration instead of exiting 2, while `--` alone and
`migrate extra --help` exit 0 without migrating; see the
[Service v3 migration table](https://github.com/faustbrian/go-service/blob/v3.0.0/docs/adoption.md#migrating-to-v3).
The whole service definition moves to go-service/v3, not only the migrate
command. A definition that also configures maintenance commands, or
any command that declares secret options, adopts the command-error
classification go-service introduced in 1.1.4, so those commands' failure exit
codes can change; review the
[go-service changelog](https://github.com/faustbrian/go-service/blob/v3.0.0/CHANGELOG.md)
from 1.1.3 through 3.0.0 for that case.

The `public.migrations` ledger, row format, checksum framing, schema
fingerprint, namespaced and command tables, and the advisory lock key are
unchanged. V3 and v4 runners contend for the same lock, so a mixed rollout
cannot run two schema owners concurrently, and no ledger alignment is needed.
Roll back by reverting the consumer revision as a whole.

## v4 to v5

Version 5 drives the PostgreSQL backend with pgx v5 instead of `database/sql`.
`postgres.New` takes a caller-owned `*pgxpool.Pool` in place of a `*sql.DB`;
the options, `Backend` methods, command session API and error categories are
otherwise unchanged. Replace `/v4` with `/v5` in imports and construct the
backend from the pool the application already owns, for example
`pgxpool.New(ctx, url)` or the pool behind a `go-postgres` client, instead of
opening a `database/sql` handle through `github.com/jackc/pgx/v5/stdlib`. The
module no longer depends on Goose or any `database/sql` driver. Unwrapped
driver causes are now pgx and pgconn errors; `database/sql` sentinels such as
`sql.ErrConnDone`, `sql.ErrTxDone` and `driver.ErrBadConn` no longer appear in
error chains.

Each session leases one pool connection from `Acquire` until `Release`, as it
leased one `*sql.Conn` before. A pool must allow one connection per concurrent
session plus any concurrent `Inspect`, `InspectObjects` or `ReadLedger` call;
`pgxpool` defaults to the larger of four and the CPU count. A session whose
lock, rollback or statement-timeout state is uncertain is closed and removed
from the pool instead of being returned. Rollback and that close now run on a
detached 30-second deadline instead of a context-free `database/sql` call.
When caller cancellation interrupts a statement, pgx closes the connection and
PostgreSQL ends the session with its transaction, `statement_timeout` and
advisory lock. Cleanup and `Release` then discard that session without adding
a cleanup or lock-release failure, so a cancelled `Runner` operation reports
only the cancellation.

Migration SQL still runs without arguments, so pgx sends it with PostgreSQL's
simple query protocol and a multi-statement file executes as one request,
exactly as it did through the `database/sql` adapter. Calling `Apply` or
`Rollback` with a zero `Migration` now returns `migrations.ErrInvalidVersion`
rather than an internal adapter error.

The `public.migrations` ledger, row format, checksum framing, schema
fingerprint, namespaced and command tables, and the advisory lock key are
unchanged. V4 and v5 runners contend for the same lock and share history
without alignment. Roll back by reverting the consumer revision as a whole.
The `testdata/interop` harness proves this against the published v4.0.0: v5
reads, plans, recovers, appends to and resumes a ledger, namespaced ledger and
command that v4 wrote, and both majors contend for one lock.

A ledger row whose `finished_at` is PostgreSQL `infinity` or `-infinity` is
rejected with `migrations.ErrInvalidRecord`. The package never writes such a
value; it can only come from a direct ledger edit.

### Migrating dependents

go-idempotency, go-lease, go-rate-limit, go-authorization and
go-queue-control-plane export go-migrations v4 types, such as
`GoMigration() migrations.Migration` and `MigrationSource()
migrations.Source`. A v5 consumer cannot use those values, so each library
needs a new major release that moves to v5. Release in this order:

1. go-migrations v5.
2. go-idempotency, go-lease, go-rate-limit, go-authorization and
   go-queue-control-plane.
3. go-scheduler.
4. The applications.

Checksums are identical across v4 and v5. Rebuilding a v4 migration with v5
`NewMigration` from the same version, name, transaction mode and SQL yields
the same checksum, so already-applied history stays valid without alignment.

Choose the next guide in the [documentation index](README.md)
or return to the [repository entry](../README.md) for installation and ownership.
