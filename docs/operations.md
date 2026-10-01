# Operations and recovery

## Deployment sequence

1. Build one immutable application image containing migrations.
2. Run CI validation and review the dry-run plan.
3. Run one dedicated Kubernetes migration Job from that image.
4. Wait for successful completion.
5. Roll out services only after the Job succeeds.

Concurrent jobs are safe: they serialize on a stable PostgreSQL advisory lock
and re-read history after acquiring it. Service startup must not invoke `Up`.

When the application uses `service`, register `adapter.Command()` as
`service.Commands.Migrate`. Its load and preparation callbacks must construct
only the migration source, backend, runner, and explicitly transferred resource
components. The adapter runs exactly the caller-selected operation as one-shot
work and cleans those components afterward; it does not retry or choose between
planning, status, application, rollback, baseline, or recovery.

Set a job deadline and review the backend's finite defaults: 30 seconds for
advisory-lock acquisition, five minutes for each migration statement, and ten
minutes for primary work in each session or schema-inspection operation. That
budget includes waiting for another caller to release serialized session
ownership. Best-effort statement-timeout restoration and lock release use
separate positive finite cleanup budgets of at most 30 seconds each. Use
`WithLockTimeout`, `WithStatementTimeout`, and
`WithOperationTimeout` when reviewed work needs different finite budgets.
Overrides must remain positive, and statement and operation timeouts must be at
least one millisecond; zero cannot silently disable a bound. No-transaction
execution restores the database-
or role-level timeout after each attempt. Lock polling respects cancellation.
Transactional cancellation rolls back both SQL and ledger. No-transaction
cancellation can leave partial effects and therefore leaves a dirty record.
Loss of the lock-owning connection releases PostgreSQL advisory ownership; the
failed operation still returns an error, and a later job must reacquire the lock
and revalidate complete history before retrying or recovering.

Go's standard `database/sql` commit, rollback, row-close, and connection-close
APIs cannot accept a context once driver finalization or cleanup begins. Use a
maintained driver with finite PostgreSQL server and network timeouts, supervise
the migration process with a hard termination budget, and treat deadline
expiry during finalization as uncertain until database and ledger state are
inspected.

Ledger reads retain at most 4,097 records and 16 MiB of aggregate text. Schema
inspection retains at most 10,000 objects, 4 KiB per identity, 1 MiB per
definition, and 16 MiB of aggregate text. `ErrResourceLimit` identifies these
fail-closed outcomes. A database driver can allocate the current field before
the package validates it, so use a maintained driver and retain process memory
limits.

Source loading independently defaults to ten minutes and can be configured
with a positive `WithSourceTimeout` override. Custom sources and backends must
honor operation contexts and return at most 4,096 migrations and 4,097 records.

## Dry run and status

Use `Runner.Plan` immediately before a change window and inspect every step.
Use `Runner.Status` for baseline, applied, dirty, and pending state. Both calls
take the same lock as execution, so they are consistent snapshots, but state may
change after the call returns.

## Dirty recovery

Never edit the ledger manually. Stop deployment, inspect the exact migration
SQL and database catalog, then choose one outcome:

- Effects are complete: create `RecoveryMarkApplied` using the source version
  and checksum.
- Effects are fully removed: create `RecoveryMarkRolledBack` using the same
  identity.

Recovery locks, revalidates source and ledger, requires exactly the matching
dirty row, and persists the decision. If the outcome is uncertain, do nothing
until it is proven.

## Rollback and disaster recovery

`Runner.Down(ctx, n)` rolls back exactly `n` clean migrations newest-first and
never crosses a baseline. It fails before execution if any selected migration
has no `Down` section. Prefer a forward repair migration for destructive or
widely deployed changes.

For database restore, restore schema and `public.migrations` from the
same consistent backup. Deploy the image containing the exact corresponding
source history, run status, and compare checksums before any execution. Never
combine a restored ledger with a newer schema or reconstruct rows by hand.

## Ledger name upgrade

The owned ledger is `public.migrations`. Before upgrading, stop all older
runners and manually rename the existing Go ledger to `migrations` in `public`,
preserving its rows, constraints, indexes, and grants. Verify the existing
history with status before executing migrations. V3 does not discover, rename,
or read any other ledger table. Starting v3 before aligning the ledger would
create an empty history and could replay already-applied migrations.

Do not run older binaries after upgrading: they target a different table.
Resolve any existing `public.migrations` ownership conflict before the manual
rename; never merge histories. Existing Laravel history must be relocated first
as specified in the [baseline runbook](laravel-baseline.md).
