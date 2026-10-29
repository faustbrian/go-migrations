# Changelog

All notable changes are documented here. The project follows Keep a Changelog
and Semantic Versioning.

## Unreleased

These changes form the v5.0.0 major.

### Changed

- Drive the PostgreSQL backend with pgx v5. `postgres.New` takes a
  caller-owned `*pgxpool.Pool` instead of a `*sql.DB`; to migrate, replace
  `/v4` with `/v5` in imports and pass the application's pgx pool, for example
  from `pgxpool.New`, instead of a `database/sql` handle opened through
  `pgx/v5/stdlib`. Options, `Backend` and command-session methods and error
  categories are otherwise unchanged. Each session still leases one connection
  from `Acquire` until `Release`, so size the pool for concurrent sessions plus
  inspection and discovery. Unwrapped driver causes are now pgx and pgconn
  errors; `database/sql` sentinels such as `sql.ErrConnDone`, `sql.ErrTxDone`
  and `driver.ErrBadConn` no longer appear in error chains.
- Run transaction rollback and the close that discards an uncertain session
  on a detached 30-second deadline instead of context-free `database/sql`
  calls. A transaction whose connection pgx already closed after caller
  cancellation no longer adds a rollback failure to a pure cancellation, and
  a failed baseline rollback now discards the session instead of returning it
  to the pool.
- Return `migrations.ErrInvalidVersion` for a zero `Migration` passed to
  `Apply` or `Rollback` instead of an internal adapter error.
- Move the module to `github.com/faustbrian/go-migrations/v5`. The ledger,
  checksum framing, schema fingerprint and advisory lock key are unchanged, so
  v4 and v5 runners serialize on the same lock and share history without
  alignment. Multi-statement migration SQL still executes as one request over
  the simple query protocol.

### Removed

- Remove the internal Goose execution adapter and the `pressly/goose`,
  `database/sql` and `go-sqlmock` dependencies. Migration SQL runs directly on
  the session's pgx transaction or lock connection with the same redacted
  execution errors.

## 4.0.0 - 2026-10-22

### Removed

- Remove the `migrationsservice` package and the Service v1 adapter binding,
  together with the `NewV2`, `OptionsV2`, `ExecutionV2` and `AdapterV2` names.
  `adapters/service` keeps one binding on `github.com/faustbrian/go-service/v3`
  as `New`, `Options` and `Execution`, so migrate commands share the fleet's
  single Service lifecycle. The adapter adds no behavior; migrate-only
  definitions keep their exit codes for ordinary invocations. Parent
  cancellation is a clean exit only when cleanup succeeds, so a failed
  shutdown, shutdown timeout or startup rollback under cancellation now keeps
  a non-zero code. The whole service definition moves to Service v3; see the
  compatibility guide for the argument-grammar changes and for commands with
  maintenance or secret options, whose exit codes can change.

### Changed

- Move the module to `github.com/faustbrian/go-migrations/v4`. The ledger,
  checksum framing, schema fingerprint and advisory lock key are unchanged, so
  v3 and v4 runners serialize on the same lock and share history without
  alignment.

## 3.2.2 - 2026-10-22

### Fixed

- Retain advisory-lock query failures when caller cancellation arrives at the
  same time. Preserve explicit transaction rollback failures alongside the
  original apply/down failure, and discard uncertain physical sessions on
  release. Already-completed transactions do not add spurious cleanup errors.
  Migration SQL, history, checkpoints, timeouts and retry policy are unchanged.

## 3.2.1 - 2026-10-22

### Fixed

- Preserve pure context cancellation versus cancellation joined with independent
  migration failures. PostgreSQL operation categories no longer add synthetic
  causes; SQL execution retains redacted independent failure branches while
  preserving `errors.Is` categories and hiding SQL and driver diagnostics.
  No migration SQL, stored history, checkpoints or retry policy changes.

## 3.2.0 - 2026-10-22

### Added

- Add PostgreSQL namespaced histories through `Backend.ForNamespace`, bounded
  read-only discovery through `ReadLedger`, atomic initial installation through
  `InstallFresh`, and retained native migration commands through `AcquireCommand`.
  Independent histories retain their original versions/checksums and share the
  native advisory lock. Default runners continue to own `public.migrations`.
  Resumable commands commit each batch's SQL, ledger and durable receipts
  together; exact retries reconcile the original prefix without replaying it.
  These APIs do not adopt existing foreign histories or accept nontransactional
  command batches. Retain command records while recovery/replay is possible.

### Changed

- Publish the current source and documentation with a fresh immutable tag.
- Build with Go 1.27.3 to address GO-2026-6809 in `crypto/x509`.
  Consumers should use Go 1.27.3 or newer.

## 3.1.0 - 2026-10-15

### Added

- Add `adapters/service.NewV2` with native Service v2 configuration, component
  and command types. Existing Service v1 adapters remain supported. Both
  bindings retain the caller-selected runner operation and migration-only
  dependency lifecycle without changing migration SQL or history.

### Documentation

- Clarify loaded-source versus locked-ledger snapshots, partial durable outcomes
  and cleanup errors, operator-asserted recovery, selected-catalog fingerprint
  limits, and caller-owned lifecycle and diagnostic boundaries.

## 3.0.2 - 2026-10-07

### Changed

- Adopt correlation 1.1.2 for the migration service dependency boundary.
- Adopt service 1.1.2 and its maintenance snapshot security patch.
  Migration adapter APIs and one-shot lifecycle behavior remain unchanged.

## 3.0.1 - 2026-10-01

### Changed

- Adopt service 1.1.1 and its patched default UUID generator. Entropy
  failures retain generation classification without exposing reader
  diagnostics through default correlation errors.
- Update the internal Goose adapter dependency to 3.28. PostgreSQL
  migrations retain their caller-owned connections, transaction handling,
  locking, and `public.migrations` ledger.
- Select pgx 5.11 for the PostgreSQL driver. Review connection strings for
  its libpq-compatible parsing changes and date/time scans for stricter
  PostgreSQL range validation. Text-decoded `timestamptz` values now use
  `ScanLocation` or the local time zone; their instants remain unchanged.
- Applications sharing the pgx dependency must add `TypeMap` to custom
  `pgx.Rows` implementations, including mocks. The migrations API continues
  to accept caller-owned `database/sql` connections.

## 3.0.0 - 2026-10-01

### Changed

- Move the module to `github.com/faustbrian/go-migrations/v3`. Consumers
  must update imports to `/v3` and stop older runners before upgrading.
- Use only `public.migrations` as the PostgreSQL ledger. Stop older binaries
  and manually align existing Go history before starting v3; other table names
  are neither discovered nor renamed. Starting with an unaligned ledger can
  replay migrations. Relocate Laravel history first and re-review its baseline.

## 2.0.0 - 2026-09-28

### Changed

- Discard dedicated PostgreSQL connections after uncertain lock acquisition or
  unlock failures instead of returning potentially locked sessions to the pool.
  Reject malformed stored durations before nontransactional rollback marks a
  clean ledger row dirty.
- Introduce secure-default behavior for the
  `github.com/faustbrian/go-migrations/v2` module path. V1 consumers must add
  `/v2` to migrations imports and provide a cancellation-aware, bounded
  `SourceFileSystem`; the persisted ledger contract remains unchanged.
- Default PostgreSQL advisory-lock acquisition to 30 seconds and migration
  statements to five minutes, while allowing only positive finite overrides.
- Redact migration SQL execution and PostgreSQL driver diagnostics from default
  errors and observer output while retaining stable error classifications.
- Add a versioned repository threat model with owned accepted risks and an
  explicit major-release boundary.
- Require source providers to accept cancellation and enforce caller-supplied
  directory and file budgets before returning data. Revalidate at most 4,096
  files, a 4 KiB source root, 1 MiB of aggregate filename data, 255-byte
  canonical names, and 16 MiB of aggregate migration-file content per complete
  source, with a finite configurable complete-load timeout.
- Reject custom source and backend histories above 4,096 migrations or 4,097
  records before planner and status allocation, and redact baseline
  fingerprints from observer failures.
- Reject persisted millisecond durations that cannot fit `time.Duration`
  before rollback or recovery can consume or remove the affected ledger row.
- Bound retained PostgreSQL ledger and schema-catalog state, apply a finite
  ten-minute default to primary database-operation work, and make contention
  for serialized session ownership cancellation-aware. Standard-library
  context-free transaction finalization and cleanup remain an owned risk.
- Reject migration, baseline, and ledger-record versions above PostgreSQL's
  signed `bigint` range before Goose or database conversion.

## 1.1.0 - 2026-09-09

### Changed

- Add `adapters/service` as the canonical one-shot migration command adapter
  and retain `migrationsservice` as a compatibility facade with unchanged
  generic signatures, named identities, errors, lifecycle, and ownership.
- Document and characterize left-to-right Runner and PostgreSQL backend option
  application, first-error termination, nil rejection, last-option precedence,
  and I/O-free caller-owned construction.

- Adopt the `go-library-tools` v1.4.0 schema-v2 cohesion contract and local
  `make cohesion` gate without changing migration APIs or runtime behavior.
- Pin reusable CI to the immutable v1.4.0 W14-enforcement workflow and enforce
  cohesion metadata in the repository's required CI contract.

- Replace the repository-local verification implementation with the immutable
  `go-library-tools` contract while retaining package-owned mutation evidence,
  API documentation baselines, PostgreSQL coverage, and engine boundaries.

- Resolve `github.com/faustbrian/go-cli`, `github.com/faustbrian/go-correlation`,
  `github.com/faustbrian/go-identifier`, and `github.com/faustbrian/go-service`
  v1.0.0 against their published module identities so dependency verification
  matches public consumers.

### Documentation

- Complete the stable-v1 entry point with the executable deployment example,
  current package and companion map, lifecycle and ownership contract,
  root-tag and compatibility policy, and support, security, troubleshooting,
  and API navigation.

- Correct the root security policy to identify the latest published `v1`
  release as supported instead of retaining pre-stable guidance.

- Publish the module's family, package selection, ownership, lifecycle, and
  support boundaries, and link to the immutable v1.4.0 ecosystem guidance.

- Replace archived monorepo links and completed execution artifacts with a
  standalone, human-oriented documentation structure.

## 1.0.0 - 2026-08-26

### Changed

- Upgrade `moby/go-archive` and `golang.org/x/crypto` to their current
  security-fixed releases and reconcile the resulting indirect dependency
  graph.

- Exclude intentional nested modules from root local-proxy archives so local,
  bootstrap, CI, and public module checksums describe the same source
  boundary.

- Track the pinned documentation-tool lockfile so clean CI checkouts install
  the exact validated cspell dependency.

- Reconcile standalone dependency checksums against deterministic current
  module archives so CI, local verification, and release consumers resolve
  identical content.

- Harden standalone documentation validation with deterministic spelling and
  link checks, package-specific documentation gates, and repository-local
  contributor guidance.

### Documentation

- Correct stale package, standalone, and authoritative-source links in public
  documentation.

### Documentation

- Link the package README to package-owned documentation.

### Changed

- Publish the module from its standalone `github.com/faustbrian/go-migrations` identity while preserving its documented API and behavior.
- Replace obsolete owned-module pseudo-version pins with the monorepo's local
  `v0.0.0` source-proxy coordinates; release tooling continues to emit exact
  `v1.0.0` dependency versions.
- Remove unused CLI-related indirect dependencies from canonical module
  metadata.
- Pin owned sibling modules to exact resolvable main pseudo-versions so
  standalone and clean external consumers use immutable dependency content.

- OpenTelemetry API dependencies now use 1.44.x consistently after adding the
  service command adapter.

### Added

- A `migrationsservice` adapter for the standard one-shot `migrate` command,
  caller-owned runner construction and operation selection, and explicit
  migration-only resource cleanup.
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

- Run the engine-neutral boundary check from the release API gate and restrict
  its scan to public Go documentation snapshots so the binary API compatibility
  baseline cannot produce a false Goose leak.
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
