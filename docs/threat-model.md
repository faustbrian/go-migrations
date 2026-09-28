# Threat model

**Model version:** 1.1

**Applies to:** `github.com/faustbrian/go-migrations/v2` source

**Reviewed:** 2026-09-27

**Owner:** `go-migrations` maintainers

This model covers migration parsing, planning, PostgreSQL locking and ledger
state, SQL execution, baselines, recovery, service integration, diagnostics,
and release automation. The [security guidance](security.md) provides operator
controls, and the repository [security policy](../SECURITY.md) defines private
reporting.

Released v1.1.0 retains its published behavior. Every v2 public release must
pass its release gates and direct-consumer migration checks before publication.

## Assets and required properties

- Canonical migration source and SHA-256 identity must not gain aliases.
- The owned PostgreSQL ledger must preserve ordered, checksum-bound history and
  explicit dirty outcomes across retries, crashes, and mixed executions.
- Advisory-lock ownership, transaction ownership, and cleanup must remain
  connection-bound and cancellation-aware.
- Migration SQL, connection data, ledger contents, schema definitions, and
  driver diagnostics must not enter default errors or observer output.
- Input-controlled CPU, memory, row counts, file counts, retries, waits, and
  database work must have explicit finite bounds.
- Published v1 source, API, and persisted formats must remain available while
  the incompatible secure defaults are prepared under `/v2`.

## Trust boundaries and attacker-controlled inputs

| Boundary | Untrusted or caller-controlled material | Package control |
| --- | --- | --- |
| Filesystem source | Root, entry names, entry count, file bytes, encodings, directives, and filesystem errors | Provider-enforced cancellation and budgets, package-side limit revalidation, canonical names and directives, UTF-8 and NUL rejection |
| Migration values | Version, name, transaction mode, up SQL, and down SQL | Immutable validated values, 255-byte name limit, aggregate SQL byte limit, canonical checksum |
| PostgreSQL driver | Connection failures, row values, result metadata, catalog definitions, latency, and diagnostic errors | Qualified ledger SQL, parameterized values, stable error categories, redacted default error strings |
| Migration SQL execution | Trusted deployment SQL and database result | Caller-owned transaction policy, finite statement timeout, dirty-state persistence for no-transaction work, SQL-error cause redaction |
| Locking and lifecycle | Competing jobs, cancellation, connection loss, and cleanup failure | Connection-bound advisory lock, post-lock revalidation, finite acquisition and release timeouts |
| Baseline and recovery | Reviewed fingerprints, dirty outcomes, and operator decisions | Serializable baseline transaction, checksum-bound recovery, explicit failure on ambiguity |
| Observability | Operation, phase, version, duration, and failures | Structured events without SQL; observer panics are contained |
| Build and release | Dependencies, actions, generated API evidence, tags, and maintainer credentials | Shared pinned security gates, immutable v1 baseline, reviewed v2 release metadata |

Migration files and explicit `Migration` values are trusted deployment
artifacts with the privileges of the supplied database role. Filesystem
implementations, database drivers, PostgreSQL servers, deployment systems, and
observers are separate caller-owned trust boundaries.

## Controls

- A failed advisory-lock acquisition or unlock leaves server lock ownership
  uncertain. The dedicated physical connection is discarded through
  `database/sql` rather than returned to its pool. Definite contention and
  successful unlock retain ordinary healthy pooling. Nontransactional rollback
  rejects an unrepresentable stored duration in the atomic ledger update's
  predicate, before a clean row becomes dirty or down SQL executes.
- `SourceFileSystem` receives the operation context and inclusive directory and
  file budgets. Implementations must apply them before retaining or returning
  data; the package revalidates every result before sorting, path handling, or
  parsing. A complete load has a finite ten-minute default and positive-only
  override. The source root is capped at 4 KiB. One load accepts at most 4,096
  entries, 255-byte canonical names, 1 MiB of aggregate filename data, 16 MiB
  per file, and 16 MiB of aggregate migration-file content.
- Planning and status reject more than 4,096 migrations or 4,097 records before
  proportional allocation, then validate the complete ordered source and
  ledger, failing closed on duplicate, deleted, renamed, reordered, dirty, or
  checksum-mismatched history.
- Migration, baseline, and ledger-record constructors reject names above 255
  bytes and versions outside PostgreSQL's positive signed `bigint` range before
  checksum work, execution, or persistence adapters can convert them.
- PostgreSQL ledger queries qualify `public`, use parameters for values, and
  retain explicit transaction ownership. A ledger read retains at most 4,097
  records and 16 MiB of aggregate text. Persisted millisecond durations must
  fit `time.Duration` before rollback or recovery mutates the affected state.
  Applied recovery also bounds the computed elapsed milliseconds atomically;
  out-of-range elapsed history remains dirty and readable.
  Migration SQL is the only deliberate raw-SQL execution surface.
- Schema inspection retains at most 10,000 objects, with 4 KiB identities,
  1 MiB definitions, and 16 MiB of aggregate identity and definition text.
- Advisory-lock acquisition defaults to 30 seconds. Migration statements
  default to five minutes. Primary session and schema-inspection work,
  including waiting for serialized session ownership, defaults to ten minutes.
  Positive finite overrides cannot disable any bound or select PostgreSQL's
  disabled zero timeout. Detached best-effort statement-timeout restoration
  and lock release each have a separate finite cleanup budget of at most 30
  seconds.
  A failed session-timeout policy change or restoration taints the physical
  session; release attempts unlock but discards it rather than returning it to
  the caller's pool, even when unlock succeeds.
- Transactional SQL and ledger writes commit or roll back together.
  No-transaction execution writes dirty state before SQL and requires explicit
  checksum-bound recovery after an uncertain result.
- Migration SQL execution errors expose only `ErrExecutionFailed` and context
  cancellation classification. PostgreSQL operation errors render a fixed
  operation and `ErrDatabaseOperationFailed`; inspecting their wrapped driver
  cause is an explicit diagnostic boundary.
- Observer events carry operation, phase, version, duration, and a redacted
  error. They never carry migration SQL, names, checksums, ledger rows, schema
  definitions, or connection strings.

## Open release-blocking findings

No known unowned Critical or High finding remains in the v2 source.
The context-free standard-library transaction and cleanup boundary is owned as
MIGRATIONS-RISK-007. Publication requires release-gate and direct-consumer
migration evidence; the v1-to-v2 adoption boundary is recorded in compatibility
guidance.

## Accepted risks

| ID | Risk | Owner | Rationale | Mitigation | Review condition |
| --- | --- | --- | --- | --- | --- |
| MIGRATIONS-RISK-001 | Reviewed migration SQL can perform every operation granted to the deployment role. | Deploying application owners | Arbitrary reviewed DDL and data migration are the module's purpose; parsing SQL semantics would be incomplete and unsafe. | Treat migrations as code, restrict review and repository access, use a least-privilege dedicated role, inspect the plan, back up data, and run one dedicated job. | Reassess when the execution model, database engine, privilege profile, or source trust changes, or after any unauthorized migration incident. |
| MIGRATIONS-RISK-002 | A no-transaction statement can leave partial database effects when it fails or the process dies. | `go-migrations` maintainers and deploying operators | PostgreSQL requires no-transaction mode for operations such as concurrent index creation; those effects cannot be rolled back atomically with the ledger. | Persist dirty state first, keep each direction to one command, stop deployment, inspect the catalog, and use checksum-bound recovery only after proving the outcome. | Reassess when PostgreSQL adds transactional support, the dirty-state protocol changes, or a partial-effect recovery fails. |
| MIGRATIONS-RISK-003 | Explicit `errors.Is` or `errors.As` inspection of a PostgreSQL operation error can reach the original driver error, which may contain database diagnostics. | Deploying application owners | Driver classifications are needed for controlled diagnosis and retry decisions, while ordinary formatting remains redacted. | Never serialize or log the unwrapped cause by default; restrict diagnostic access and retention; migration SQL execution causes are discarded entirely except for context classification. | Reassess when adding a driver, observability adapter, automatic retry, or public error serialization, or after diagnostic disclosure. |
| MIGRATIONS-RISK-004 | Administrators, other frameworks, or a compromised PostgreSQL server can mutate schema outside the package advisory lock. | Deploying application owners | The advisory lock coordinates this package's jobs but cannot authorize or serialize unrelated database actors. | Restrict database access, isolate deployment windows, compare schema fingerprints, audit DDL, and restore schema and ledger from one consistent backup. | Reassess when another migration system shares the database, database privileges change, or unexplained schema drift occurs. |
| MIGRATIONS-RISK-006 | A database driver may allocate one hostile ledger or catalog field before the package can enforce its per-field and aggregate retention budgets. | Deploying application owners | `database/sql` transfers the current field before package code can validate its length; rejecting standard drivers would remove the supported database boundary. | Use a maintained driver, a trusted PostgreSQL endpoint, role and transport controls, server-side statement limits, and process memory limits. The package validates each scanned row before appending it and bounds primary operation work. | Reassess when adding a driver, accepting an untrusted database endpoint, after database-response memory exhaustion, or if `database/sql` adds bounded field reads. |
| MIGRATIONS-RISK-007 | A PostgreSQL driver can block inside transaction commit, rollback, row close, or connection close after the surrounding context expires. | `go-migrations` maintainers and deploying operators | Go's standard `database/sql` commit, rollback, and cleanup APIs do not accept a context once driver cleanup begins; replacing the portable database boundary with driver-specific internals would weaken compatibility. | Use a maintained context-aware driver and finite PostgreSQL server and network timeouts, run migrations in a supervised process with a hard termination budget, and treat deadline expiry during transaction finalization as an uncertain outcome requiring database and ledger inspection before retry. | Reassess when `database/sql` adds context-aware finalization, supported drivers expose a portable bounded transaction API, a driver or network timeout fails to terminate cleanup, or an operation remains stuck past its supervisor budget. |

No accepted risk permits credentials, connection strings, migration SQL, raw
ledger values, or schema definitions in default diagnostics.

## Change and release review

Review this model whenever parsing, checksums, SQL execution, timeouts,
transactions, locking, ledger schema, baselines, recovery, driver behavior,
diagnostics, dependencies, or release automation changes. A release is blocked
by any open Critical or High finding, an unbounded attacker-controlled path,
loss of dirty-state recovery, loss of v1 compatibility evidence, or a default
diagnostic that exposes protected material.
