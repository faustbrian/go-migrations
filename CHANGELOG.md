# Changelog

All notable changes are documented here. The project follows Keep a Changelog
and will use semantic versioning after the first stable release.

## Unreleased

### Added

- Engine-neutral immutable migration, plan, status, baseline, recovery, event,
  backend, session, and runner contracts.
- Canonical embedded SQL format with SHA-256 identities and strict parsing.
- PostgreSQL advisory locking, owned ledger, transactional and explicit
  no-transaction execution, timeouts, and schema fingerprint baselines.
- Explicit dirty-state recovery and deterministic rollback planning.
- PostgreSQL 14 through 18 integration coverage, concurrency tests, fuzzing,
  and persistence-boundary fault injection.
- Reusable engine conformance tests, public API snapshots, embedded/Kubernetes
  examples, operational runbooks, and release automation.
- MIT open-source license.
- Immutable v1 migration and ledger compatibility corpus with cross-version
  PostgreSQL upgrade tests.
- Production-shaped Laravel baseline fixtures for empty, exact, drifted,
  partial, and unexpectedly advanced schemas.
- Process-death coverage for lock waiters, transactional SQL, dirty execution,
  clean-ledger writes, and connection loss.
- Native parser, source, planner, status, and fingerprint benchmark baselines.
- Goose 3.26 and 3.27 adapter upgrade matrix against persisted v1 history.

### Security

- Fail-closed validation for modified, renamed, deleted, reordered, malformed,
  partial, dirty, or baseline-conflicting history.
- Upgrade `golang.org/x/text` to the latest fixed release to remove
  `GO-2026-5970` from reachable pgx-backed inspection paths.

### Fixed

- Bind owned-ledger preparation to the advisory-lock session so first-run
  migration works with a database pool limited to one connection.
- Length-prefix canonical up and down SQL before hashing so distinct section
  boundaries cannot produce the same migration checksum.
- Enforce UTF-8, NUL-byte, and size limits in the public migration constructor,
  preventing callers from bypassing canonical file validation.
- Reject an explicit whitespace-only down section instead of representing an
  irreversible migration as a runnable rollback.
- Reject sub-millisecond PostgreSQL statement timeouts instead of truncating
  them to PostgreSQL's timeout-disabled `0ms` value.
- Reject the all-zero checksum sentinel during parsing so every successfully
  parsed checksum is valid for records and baselines.
- Limit migration versions to positive signed 64-bit values so plans cannot
  contain identities that the owned PostgreSQL ledger cannot persist.
- Qualify every ledger operation with the `public` schema so a hostile
  `search_path` cannot create a separate migration history.
- Reject ledger rows whose dirty flag disagrees with completion state, even if
  a pre-existing table is missing the package-owned constraint.
- Persist the owned PostgreSQL contract instead of Goose identity in migration
  rows so adapter upgrades cannot leak into ledger semantics.
- Keep replaceable adapter identity out of errors returned through the public
  backend contract.
