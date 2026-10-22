# Package map

[Documentation index](README.md) · [Repository entry](../README.md)

This map follows the current source modules, not a synchronized release.
Use the repository README for published installation versions; an unpublished
source major is not installable merely because its package appears here.
Internal packages and non-releasable fixtures are intentionally excluded.

## github.com/faustbrian/go-migrations/v4

`migrations` is an engine-neutral database migration runtime with a PostgreSQL backend. It owns migration identity, planning, status, locking, checksums, baselines, recovery, and the `public.migrations` ledger. Goose is an internal, pinned SQL execution detail and never appears in the public API.

| Import path | Responsibility |
| --- | --- |
| [`github.com/faustbrian/go-migrations/v4`](../baseline.go) | Use for immutable migration values, filesystem sources, planning, status, baselines, recovery, and engine-neutral runner coordination. |
| [`github.com/faustbrian/go-migrations/v4/adapters/service`](../adapters/service/service.go) | Use to adapt an explicitly constructed runner and operation to the standard one-shot Service v2 migrate command. |
| [`github.com/faustbrian/go-migrations/v4/conformance`](../conformance/suite.go) | Use when implementing or replacing a migration engine to verify the complete public runner contract. |
| [`github.com/faustbrian/go-migrations/v4/postgres`](../postgres/backend.go) | Use for PostgreSQL advisory locking, ledger persistence, transactional execution, recovery, and schema fingerprinting. |

Choose the next guide in the [documentation index](README.md)
or return to the [repository entry](../README.md) for installation and ownership.
