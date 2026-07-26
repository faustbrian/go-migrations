# Architecture and engine contract

Applications import the root package and an engine backend. The root package
loads immutable migrations, validates history, builds deterministic plans, and
coordinates execution. `postgres.Backend` owns PostgreSQL advisory locking,
schema inspection, ledger persistence, transactions, and statement timeouts.
The internal Goose adapter receives already-validated immutable migrations and
executes SQL on the caller-owned transaction or connection.

All operations follow this order:

1. Load and validate the complete source.
2. Acquire one physical, connection-bound engine session.
3. Prepare the owned ledger while holding the lock.
4. Read and revalidate the complete ledger.
5. Plan or execute deterministic steps.
6. Release using a bounded context detached from job cancellation.

`Backend` acquires a `Session`. A session must represent exclusive ownership and
must bind ledger preparation, reads, SQL execution, and release to the same
physical connection. This ordering supports a pool limited to one connection
without weakening first-run serialization. `Apply` and `Rollback` return
validated root-package `Record` values. Optional baseline and recovery
capabilities are discovered by the runner. Engine errors may be wrapped, but
engine types must never cross the root public boundary.

Structured `Event` values contain operation, phase, version, duration, and an
error. SQL is deliberately excluded. Adapters may translate events to logging
or telemetry without influencing execution; observer panics are contained.

`database/sql` integration avoids a dependency on a specific pool wrapper.
Applications using `postgres` should expose its underlying `*sql.DB` to the
PostgreSQL backend. Neither package needs to import the other, avoiding a cycle.
`sqlc` remains service-local build tooling and is not runtime migration state.
