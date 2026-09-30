# Compatibility

The published v1 line remains available. This source contains the v2 line at
`github.com/faustbrian/go-migrations/v2`, published as v2.0.0.
The module follows semantic versioning, and root releases
use `v<version>` tags. Compatible defect and security fixes may be backported to
a supported major; incompatible behavior requires a new major version and
migration guidance in `CHANGELOG.md`.

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

The published v1 line remains available at its original module path. The v2
line preserves persisted formats but intentionally replaces plain `fs.FS`
source access with the cancellation-aware, bounded `SourceFileSystem` contract
and changes omitted PostgreSQL timeout options from unbounded behavior to finite
defaults. Consumers upgrade by adding `/v2` to migrations
imports, providing that source boundary, and reviewing the 30-second lock and
five-minute statement budgets.
`testdata/compatibility/v1` is the immutable persisted-contract upgrade anchor.
Every future supported release line must retain this fixture and add a new
fixture before changing the source format, checksum, or ledger contract. The
real PostgreSQL matrix installs the historical schema and row, then proves the
current package can plan and append work without rewriting history.

The adapter upgrade matrix currently executes the same unit and historical
ledger contract against Goose `v3.26.0` and `v3.27.1`. Removing a version or
adding a newer pin requires a changelog entry and a green persisted-ledger
scenario before release.
