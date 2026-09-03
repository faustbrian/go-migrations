# Changelog

All notable changes are documented here. The project follows Keep a Changelog
and Semantic Versioning.

## Unreleased

### Changed

- Adopt the `go-library-tools` v1.3.0 schema-v2 cohesion contract and local
  `make cohesion` gate without changing migration APIs or runtime behavior.
- Pin reusable CI to the v1.3.0 workflow and enforce cohesion metadata in the
  repository's required CI contract.

- Replace the repository-local verification implementation with the immutable
  `go-library-tools` contract while retaining package-owned mutation evidence,
  API documentation baselines, PostgreSQL coverage, and engine boundaries.

- Resolve `github.com/faustbrian/go-cli`, `github.com/faustbrian/go-correlation`,
  `github.com/faustbrian/go-identifier`, and `github.com/faustbrian/go-service`
  v1.0.0 against their published module identities so dependency verification
  matches public consumers.

### Documentation

- Publish the module's family, package selection, ownership, lifecycle, and
  support boundaries, and link to the immutable v1.3.0 ecosystem guidance.

- Replace archived monorepo links and completed execution artifacts with a
  standalone, human-oriented documentation structure.

## 1.0.0 - 2026-08-25

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
