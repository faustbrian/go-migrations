# Hardening evidence

This document maps release threats to executable evidence. Test names are part
of the review contract: removing or narrowing one requires an equivalent test,
compatibility analysis, and a changelog entry.

## Laravel baseline matrix

The real PostgreSQL suite installs production-shaped fixtures from
`postgres/testdata/laravel` and compares every candidate to the fingerprint of
`exact.sql`:

| Fixture | Expected outcome |
| --- | --- |
| `empty.sql` | Reject missing application schema |
| `exact.sql` | Record one baseline |
| `drifted.sql` | Reject changed column definition |
| `partial.sql` | Reject missing production objects |
| `advanced.sql` | Reject unexpected future objects |

`baseline fixtures fail closed outside exact Laravel schema` verifies the
Laravel migration rows byte-for-byte before and after every attempt. The
existing drift test additionally compares complete schema-object snapshots
before and after a failed attempt.

## Persistence and crash boundaries

Unit fault injection stops transactional apply and rollback at begin, timeout
setup, dirty insert, migration SQL, clean update, delete, commit, result
inspection, and timeout reset. Baseline and recovery tests stop at begin,
inspection, insert or update, commit, and result decoding. Locked runner tests
prove release still runs after preparation, ledger, planning, execution, and
observer failures.

The real process harness complements those deterministic faults:

| Boundary | Required invariant |
| --- | --- |
| Killed while waiting for the advisory lock | No ledger preparation or schema mutation |
| Lock timeout or caller cancellation | Current owner remains usable |
| Lock connection terminated by PostgreSQL | Ownership is released and retry succeeds |
| Killed inside transactional migration SQL | Schema and ledger both roll back |
| Killed after a no-transaction dirty insert | Partial effects remain visibly dirty |
| Killed during the clean-ledger update | Completed effects remain dirty until reviewed recovery |

The two dirty outcomes are recovered through checksum-bound
`RecoveryMarkRolledBack` and `RecoveryMarkApplied`; tests never repair rows
manually.

## History and engine conformance

The reusable `conformance.Run` suite executes on every supported PostgreSQL
major. It verifies initial and applied plans, pending and applied status,
idempotency, rollback, concurrent serialization, transactional failure,
no-transaction recovery, and checksum, rename, and deletion failures through
`Plan`, `Status`, and `Up`.

Root contract tests reject duplicate source versions, reordered or gapped
history, malformed records, dirty baselines, removed files, renamed files, and
checksum mutations. Parser unit and fuzz tests cover comments, dollar-quoted
semicolons, directive ordering, invalid encodings, NUL bytes, byte-order marks,
size limits, hostile filenames, and unrelated source entries.

## Upgrade compatibility

`testdata/compatibility/v1` freezes canonical source files, checksums, a
historical ledger, owned backend identity, and the adapter version used to
produce the fixture. Unit tests detect any identity change. Every PostgreSQL
matrix job installs that ledger, reads its applied state, applies the pending
no-transaction migration, and proves the historical row was not rewritten.
The engine-upgrade matrix repeats adapter and real-ledger tests against every
documented supported Goose version instead of trusting fixture metadata.

Goose remains an implementation detail. The engine-boundary gate rejects Goose
identifiers in public API snapshots, direct Goose imports outside the internal
adapter, or Goose provenance in new owned-ledger SQL.

## Release gates

CI and tagged releases run formatting, vet, lint, meaningful 100% runtime
coverage, race tests, fuzz smoke tests, vulnerability scanning, public API and
engine-boundary checks, the immutable compatibility corpus, benchmark smoke
tests, examples, reusable conformance, real process tests, and PostgreSQL 14
through 18.
