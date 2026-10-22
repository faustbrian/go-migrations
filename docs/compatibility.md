# Compatibility

[Documentation index](README.md) · [Repository entry](../README.md)

This source contains the v4 line at `github.com/faustbrian/go-migrations/v4`.
Published releases keep their original module paths.
The module follows semantic versioning, and root releases
use `v<version>` tags. Compatible defect and security fixes may be backported to
a supported major; incompatible behavior requires a new major version and
migration guidance in `CHANGELOG.md`.

The source format and PostgreSQL schema fingerprint are explicitly versioned as
v1 contracts. Ledger history must remain readable across compatible releases.
Supported PostgreSQL major versions are 14, 15, 16, 17, and 18. Current hosted
CI exercises PostgreSQL 18; it does not establish a complete supported-version
matrix. PostgreSQL majors are removed only after upstream ends support and
the removal is documented. The Go version is declared by `go.mod`.

The pinned Goose version is an internal implementation constraint, not an
application compatibility surface. Applications must not import Goose for
migration runtime behavior. The compatibility corpus records which
adapter version produced each fixture, while persisted ledger rows contain only
the owned PostgreSQL contract identity.

The published v1 line remains available at its original module path. The v2
line preserves persisted row formats but intentionally replaces plain `fs.FS`
source access with the cancellation-aware, bounded `SourceFileSystem` contract
and changes omitted PostgreSQL timeout options from unbounded behavior to finite
defaults. Consumers upgrade by adding `/v2` to migrations
imports, providing that source boundary, and reviewing the 30-second lock and
five-minute statement budgets.
`testdata/compatibility/v1` preserves v1 source identities, checksums, and
persisted row formats, with the table name manually aligned to the v3 ledger.
The original unaligned fixture remains available in published v1 and v2 tags.
Add a new fixture before changing the source format, checksum, or row contract.
The real PostgreSQL integration scenario installs the aligned schema and
historical row, then proves the current package can plan and append without
rewriting history.

Current unit and historical-ledger tests select Goose `v3.28.0` through
`go.mod`; they do not execute a multi-version Goose matrix. Historical fixture
producer versions remain recorded in the compatibility corpus. Updating the
pin requires a changelog entry and a green persisted-ledger scenario before
release.

The ledger is now only `public.migrations`. Stop old runners and manually
align existing Go history before starting v3; no other table is recognized or
excluded from schema fingerprints. Resolve any conflicting ownership first.
The aligned v1 fixture proves row compatibility, not automatic table adoption.

V3 consumers must replace `/v2` with `/v3` in Go imports. The Go API is
otherwise unchanged from v2; the ledger rename and Laravel relocation
requirement are the breaking persisted-contract changes.

## v3 to v4

Version 4 removes the `migrationsservice` package, the Service v1 adapter
binding and the `NewV2`, `OptionsV2`, `ExecutionV2` and `AdapterV2` names.
`adapters/service` now has one binding on `github.com/faustbrian/go-service/v2`:
use `New`, `Options` and `Execution`, which take the native Service v2 types.
Replace `/v3` with `/v4` in imports and move the migrate command's service
composition to Service v2 in the same change.

The `public.migrations` ledger, row format, checksum framing, schema
fingerprint, namespaced and command tables, and the advisory lock key are
unchanged. V3 and v4 runners contend for the same lock, so a mixed rollout
cannot run two schema owners concurrently, and no ledger alignment is needed.
Roll back by reverting the consumer revision as a whole.

Choose the next guide in the [documentation index](README.md)
or return to the [repository entry](../README.md) for installation and ownership.
