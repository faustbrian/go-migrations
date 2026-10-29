# Operations and recovery

[Documentation index](README.md) · [Repository entry](../README.md)

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
separate positive finite cleanup budgets of 30 seconds by default;
`WithUnlockTimeout` can replace the runner release budget. Use
`WithLockTimeout`, `WithStatementTimeout`, and
`WithOperationTimeout` when reviewed work needs different finite budgets.
Overrides must remain positive, and statement and operation timeouts must be at
least one millisecond; zero cannot silently disable a bound. No-transaction
execution restores the database-
or role-level timeout after each attempt. Lock polling respects cancellation.
Transactional cancellation before commit attempts to roll back SQL and ledger;
commit/finalization errors can have uncertain outcomes. No-transaction
cancellation after dirty marking can leave partial effects and a dirty record.
Failure before marking may leave no row, while timeout-restoration or lock-release
failure may follow already completed SQL and clean/deleted ledger state. Inspect
state before retrying: an error is not evidence that nothing committed, and
Runner results include only successful acknowledgements before the failed step.
Loss of the lock-owning connection releases PostgreSQL advisory ownership; the
failed operation still returns an error, and a later job must reacquire the lock
and revalidate complete history before retrying or recovering.

Advisory-lock acquisition retains a query failure alongside concurrent caller
cancellation. Transactional apply/down errors also retain meaningful explicit
rollback failures; still call `Release` so an uncertain physical session is
discarded. Already-completed transactions add no cleanup failure, and neither
does a session that pgx already closed after caller cancellation interrupted
a statement: PostgreSQL ends that session, aborting its transaction and
dropping its `statement_timeout` and advisory lock, so rollback, timeout
restoration and `Release` discard it without reporting a cleanup failure.
Unlock and restoration failures on a live connection are still reported.

pgx accepts a context for commit, rollback and connection close. Rollback and
the close that discards an uncertain session run on a detached 30-second
deadline, so caller cancellation cannot skip them and they cannot block
forever. Keep finite PostgreSQL server and network timeouts, supervise the
migration process with a hard termination budget, and treat deadline expiry
during commit or cleanup as uncertain until database and ledger state are
inspected.

Ledger reads retain at most 4,097 records and 16 MiB of aggregate text. Schema
inspection retains at most 10,000 objects, 4 KiB per identity, 1 MiB per
definition, and 16 MiB of aggregate text. `ErrResourceLimit` identifies these
fail-closed outcomes. pgx reads the current field into memory before the
package validates it, so connect only to trusted PostgreSQL endpoints and
retain process memory limits.

Source loading independently defaults to ten minutes and can be configured
with a positive `WithSourceTimeout` override. Custom sources and backends must
honor operation contexts and return at most 4,096 migrations and 4,097 records.

## Dry run and status

Use `Runner.Plan` immediately before a change window and inspect every step.
Use `Runner.Status` for baseline, applied, dirty, and pending state. Both calls
take the same lock as execution and validate loaded source against locked
ledger state. Source is loaded before acquisition and must remain an immutable
deployment artifact. Both calls can create the owned ledger, and neither
reserves state for a later execution call.

## Atomic initial installation

Some packages reserve shared prerequisites, such as PostgreSQL roles, which
must roll back if any later schema step fails. Load the complete immutable
history from that package's public source, then call
`backend.InstallFresh(ctx, history, packagePreflightSQL)` instead of applying
the steps individually. The native advisory lock serializes installation;
ledger creation, preflight and every migration execute in one transaction.
Each original version, name and checksum is retained. No package SQL needs to
be copied or concatenated into another migration identity.

Only reviewed transactional SQL is permitted, including the preflight. Do not
supply transaction-control statements, no-transaction migrations or SQL with
external/nontransactional effects. The combined SQL limit is 16 MiB and the
history limit is 4,096 steps. An empty source is rejected. The borrowed pool
remains open; operation, statement and lock deadlines use backend policy.

Existing nonempty history returns `ErrLedgerConflict` before preflight runs,
including an exact repeat of a completed installation. This operation cannot
upgrade or silently adopt existing state. `ErrInstallOutcomeUnknown` means
commit or post-commit ownership release was uncertain: stop, reacquire native
ownership and reconcile the exact original ledger/source before taking another
action. Never infer rollback from a connection error or blindly replay setup.
Commit and cleanup still have the supervision requirements described above.
Ordinary `Runner.Up` keeps its per-migration transactions. This API alone does
not provide a reference exact-plan/resume protocol.

## Independent migration owners

Derive a backend with `backend.ForNamespace("outbox")` before constructing the
runner or calling discovery, inspection or initial installation. This IO-free
operation copies the borrowed pool and timeout policy without changing the
original backend. The selected ledger is exactly
`public.golib_migrations_outbox`; the default remains `public.migrations`.

Namespaces contain 1..46 bytes of lowercase ASCII snake case, beginning with a
letter. Digits are permitted after the first letter; consecutive or trailing
underscores are rejected. Invalid names fail with `ErrInvalidConfig` before IO,
without normalization or PostgreSQL identifier truncation. Re-deriving selects
an absolute owner, not a nested namespace.

Keep each owner's namespace stable for its lifetime. Selection does not discover,
rename, copy or merge existing history. Changing namespaces can expose empty
history and replay SQL; it is not a migration or an authorization boundary.
Retain each source's original versions, names and checksums, even when different
owners both begin at version one. SQL still addresses the shared database;
separate histories do not make incompatible schema changes safe. Every owner
uses the same native advisory lock to serialize mutation.

Inspection and baseline fingerprints remain database-wide. Only the selected
ledger and its catalog objects are excluded; other owners' ledgers remain
visible. Review baselines after selecting the owner and stabilize other schema
changes before recording them. Restore schema and every selected ledger from
the same consistent backup. Namespace selection does not supply application
exact-plan execution, command idempotency or ambiguous-outcome reconciliation.

## Dirty recovery

Never edit the ledger manually. Stop deployment, inspect the exact migration
SQL and database catalog, then choose one outcome:

- Effects are complete: create `RecoveryMarkApplied` using the source version
  and checksum.
- Effects are fully removed: create `RecoveryMarkRolledBack` using the same
  identity.

Recovery locks, revalidates source and ledger, requires exactly the matching
dirty row, and persists the decision without checking or executing SQL effects.
MarkRolledBack returns the dirty record removed; MarkApplied returns its new
clean record. If the outcome is uncertain, do nothing
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

Choose the next guide in the [documentation index](README.md)
or return to the [repository entry](../README.md) for installation and ownership.
