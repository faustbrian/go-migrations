# Laravel-to-Go baseline runbook

This procedure adopts an existing schema without replaying Laravel migrations.
It never renames, reads, reuses, or mutates Laravel's `migrations` table.

## Review

1. Choose a positive baseline version below every future Go migration.
2. Quiesce schema-changing deployments.
3. Call `postgres.Backend.InspectObjects` against a production-like clone.
4. Review the canonical object list for tables, columns, constraints, indexes,
   triggers, policies, domain/enum types, functions, and extensions.
5. Call `Inspect` and record the lowercase fingerprint in reviewed source or
   deployment configuration. Treat a changed fingerprint as a new review.
6. Create the first Go migration with a strictly greater version.

## Dry run

Construct `NewBaseline(version, name, fingerprint)`, run status and plan, and
verify that the owned ledger is empty. Confirm the application image contains
no historical Laravel migrations and no Go migration at or below the baseline.

## Apply

Run `Runner.Baseline` in the dedicated migration Job. Under the advisory lock it
opens a serializable transaction, recomputes the schema fingerprint, compares
it byte-for-byte, inserts one clean baseline row, and commits. Drift, existing
owned history, concurrent changes, or duplicate execution fail closed.

Afterward, verify one `kind='baseline'` row in
`public.go_schema_migrations`, verify the Laravel table and row count are
unchanged, run status, then apply later Go migrations normally.

## Rollback and disaster recovery

A baseline is an adoption boundary, not executable SQL, and cannot be rolled
back. If baseline recording fails, correct drift or configuration and retry; do
not insert a row manually. If an incorrect baseline was committed, stop all Go
migration jobs and restore the database to a pre-baseline consistent backup or
perform a separately reviewed ledger correction under incident procedure.
Preserve the Laravel table throughout.
