# Goal: Stable Database Migration Runtime

## Objective

Build a production-grade open-source migration package with an engine-neutral
public contract, PostgreSQL-first execution, and a replaceable internal Goose
adapter. Applications MUST NOT import Goose or depend on Goose data structures,
file semantics, ledger schema, or errors.

## Ownership Boundary

- Own the canonical migration identity, SQL file format, source abstraction,
  plan, status, checksum, baseline, locking, ledger, errors, and runner API.
- Use Goose only as an internal execution adapter where it fits these contracts.
- Pin and audit Goose; no Goose type may cross a public boundary.
- Maintain an engine conformance suite so Goose can later be replaced without
  changing application migration code or persisted migration history.
- Integrate with `postgres` without making either package depend cyclically.
- Keep `sqlc` build-time and service-local; it is not migration runtime state.

## Migration Model

- SQL migrations are the canonical v1 format, loaded from `fs.FS` and suitable
  for `go:embed`.
- Each migration has an immutable version, name, checksum, transaction mode,
  up SQL, and down SQL where rollback is supported.
- Store state in an owned `go_schema_migrations` ledger rather than a
  Goose-owned table.
- Use PostgreSQL advisory locking to serialize migration jobs.
- Detect dirty, partial, reordered, deleted, and checksum-mutated migrations.
- Expose deterministic plan and status operations before execution.

## Existing Laravel Database Adoption

- Do not rename, reuse, or mutate Laravel's `migrations` table.
- Inspect the expected production schema and create a reviewed schema
  fingerprint or equivalent baseline assertion.
- Record one explicit Go baseline in `go_schema_migrations` only after the
  schema satisfies the approved baseline contract.
- Treat all later Go migrations as new immutable versions.
- Baseline and migration execution MUST be safe under concurrent Kubernetes
  migration jobs and MUST fail closed on drift.

## Operational Semantics

- Support transactional migrations by default and explicit no-transaction mode
  for PostgreSQL operations that require it.
- Define cancellation, statement timeout, lock timeout, retries, dirty state,
  rollback, and crash recovery precisely.
- Run migrations as a dedicated Kubernetes job before service rollout; service
  startup MUST NOT race migrations implicitly.
- Emit structured events suitable for `log` and `telemetry` integration.

## Quality Requirements

- Meaningful 100% statement coverage is required for the public contract,
  parser, planner, ledger, baseline, locking, and all failure states.
- Integration tests MUST use real supported PostgreSQL versions.
- Race and concurrent-process tests MUST exercise lock and ledger behavior.
- Fuzz tests MUST cover migration files, directives, versions, checksums, and
  malformed ledger data.
- Fault tests MUST terminate execution at every persistence boundary.

## Documentation Deliverables

- Complete public API, migration format, ledger, and engine contract docs.
- Laravel-to-Go baseline runbook with review, dry-run, rollback, and disaster
  recovery steps.
- Adoption examples for embedded migrations, Kubernetes jobs, CI validation,
  `postgres`, transactions, no-transaction migrations, and status reporting.
- Architecture, security, operations, compatibility, FAQ, contribution, engine
  replacement, and maintained `CHANGELOG.md` documentation.

## Automation And Release

GitHub Actions MUST run formatting, vetting, linting, unit and PostgreSQL matrix
tests, race tests, fuzz smoke tests, coverage enforcement, vulnerability scans,
examples, API compatibility, and migration-engine conformance tests.

## Phases

1. Specify canonical files, public API, ledger, locks, and baseline contract.
2. Implement parsing, planning, status, checksums, and owned ledger.
3. Implement the hidden Goose execution adapter and PostgreSQL integration.
4. Implement Laravel baseline tooling, drift checks, and Kubernetes workflows.
5. Complete failure injection, engine conformance, documentation, and release.

## Acceptance Criteria

- Application code imports only `migrations` contracts, never Goose.
- Existing Laravel databases can be baselined without replaying old migrations.
- Ledger, locking, checksums, dirty state, and crash recovery are deterministic.
- A replacement engine can pass the same conformance suite and persisted ledger.
- Meaningful 100% coverage and all GitHub Actions gates pass.
- Documentation is operationally complete and `CHANGELOG.md` is current.
