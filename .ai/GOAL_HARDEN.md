# Hardening Goal: Database Migrations

## Objective

Prove that migrations fail closed and remain recoverable across schema drift,
concurrent deploys, process death, engine upgrades, and legacy baselines.

## Required Audits

- Validate Laravel baseline creation against empty, exact, drifted, partial, and
  unexpectedly advanced schemas without mutating Laravel migration history.
- Kill execution before and after every lock, statement, transaction, and ledger
  write to verify recovery and dirty-state behavior.
- Run concurrent migration jobs and prove advisory-lock ownership, timeout,
  cancellation, and connection-loss semantics.
- Test transactional DDL rollback and explicit no-transaction partial failure.
- Reject duplicate, reordered, deleted, renamed, and checksum-mutated migrations.
- Exercise PostgreSQL version differences and non-transactional operations.
- Verify SQL parsing and directives with comments, dollar quoting, semicolons,
  encodings, large files, and hostile input.
- Prove the Goose adapter and any future engine satisfy the same public plan,
  execution, ledger, error, and status contracts.
- Test upgrades across supported package and Goose versions using persisted
  historical ledgers.

## Required Deliverables

- Crash-point and concurrent-runner integration harness.
- PostgreSQL version and engine conformance matrices.
- Laravel baseline fixtures representing exact and drifted production schemas.
- Immutable migration and ledger compatibility test corpus.
- Operator runbooks for baseline, deploy, rollback, dirty recovery, and disaster
  recovery.
- Security review, benchmark baselines, complete docs, and `CHANGELOG.md`.

## Release Blockers

- Any path that can apply the same migration twice or skip one silently.
- Baseline acceptance when the schema does not match the approved contract.
- Lock loss, dirty state, checksum drift, or partial execution without an
  explicit recoverable outcome.
- Leakage of Goose types or semantics through the public API or ledger.
- Missing Meaningful 100% coverage or a failing GitHub Actions gate.

## Completion Criteria

- Baseline, crash, concurrency, drift, and engine conformance suites pass.
- Race, fuzz, vulnerability, compatibility, and PostgreSQL matrix gates pass.
- Operators can recover every documented failure without editing ledger rows by
  hand under normal procedures.
- No release blocker remains and `CHANGELOG.md` is current.
