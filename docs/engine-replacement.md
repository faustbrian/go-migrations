# Replacing the execution engine

An alternative engine implements `Backend` and connection-bound `Session`
without changing root-package migrations or persisted records. Preserve the
canonical parser, checksum algorithm, planner, status model, lock ownership,
ledger schema, record validation, dirty semantics, and baseline contract.

The engine must execute transactional SQL and ledger changes atomically. For
no-transaction work it must persist dirty state before SQL and clean state only
after success. Connection or process loss at any boundary must result in either
an atomic rollback or a detectable dirty outcome. Recovery must be explicit and
checksum-bound. `Session.Prepare`, record reads, migration SQL, recovery, and
release must use the same lock-owning physical connection. Backend-specific
conformance coverage must include a pool restricted to one connection.

Before replacement, construct a `conformance.Harness` with engine-specific SQL,
runner construction, and partial-effect cleanup, then call `conformance.Run`
from the backend's real-database test. Run that shared suite with fault
injection at every persistence boundary, concurrent-process lock tests,
cancellation tests, and the supported database matrix. Test against an existing
ledger created by the prior engine. Never rewrite checksums or synthesize clean
rows to make a new engine appear compatible.

The shared suite must remain engine-neutral while covering deterministic plans,
status transitions, idempotency, rollback, dirty recovery, concurrent runners,
and checksum, rename, and deletion errors through public runner operations.
