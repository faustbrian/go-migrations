# Operations and recovery

## Deployment sequence

1. Build one immutable application image containing migrations.
2. Run CI validation and review the dry-run plan.
3. Run one dedicated Kubernetes migration Job from that image.
4. Wait for successful completion.
5. Roll out services only after the Job succeeds.

Concurrent jobs are safe: they serialize on a stable PostgreSQL advisory lock
and re-read history after acquiring it. Service startup must not invoke `Up`.

Set a job deadline, a shorter lock timeout, and a statement timeout appropriate
for the largest reviewed operation. PostgreSQL statement timeouts must be at
least one millisecond; smaller values are rejected instead of truncating to the
special disabled value of `0ms`. Lock polling respects cancellation.
Transactional cancellation rolls back both SQL and ledger. No-transaction
cancellation can leave partial effects and therefore leaves a dirty record.
Loss of the lock-owning connection releases PostgreSQL advisory ownership; the
failed operation still returns an error, and a later job must reacquire the lock
and revalidate complete history before retrying or recovering.

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

For database restore, restore schema and `public.go_schema_migrations` from the
same consistent backup. Deploy the image containing the exact corresponding
source history, run status, and compare checksums before any execution. Never
combine a restored ledger with a newer schema or reconstruct rows by hand.
