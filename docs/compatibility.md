# Compatibility

The module follows semantic versioning once a `v1.0.0` release is published.
Before that release, minor versions may change public APIs or persisted
contracts and will document changes in `CHANGELOG.md`.

The source format and PostgreSQL schema fingerprint are explicitly versioned as
v1 contracts. Ledger history must remain readable across compatible releases.
The current PostgreSQL integration matrix covers supported major versions 14,
15, 16, 17, and 18. PostgreSQL majors are removed only after upstream ends
support and the removal is documented. The Go version is declared by `go.mod`.

The pinned Goose version is an internal implementation constraint, not an
application compatibility surface. Applications must not import Goose for
migration runtime behavior. The immutable compatibility corpus records which
adapter version produced each fixture, while persisted ledger rows contain only
the owned PostgreSQL contract identity.

Before the first published module release, `v1` is the only supported package
contract. `testdata/compatibility/v1` is its immutable upgrade anchor. Every
future supported release line must retain this fixture and add a new fixture
before changing the source format, checksum, or ledger contract. The real
PostgreSQL matrix installs the historical schema and row, then proves the
current package can plan and append work without rewriting history.

The adapter upgrade matrix currently executes the same unit and historical
ledger contract against Goose `v3.26.0` and `v3.27.1`. Removing a version or
adding a newer pin requires a changelog entry and a green persisted-ledger
scenario before release.
