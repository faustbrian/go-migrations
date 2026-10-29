# Contributing

## Before Editing

1. Read [`AGENTS.md`](AGENTS.md) and the affected package's docs.
2. Run the affected package's deterministic tests as described below.
3. Identify owned dependencies and reverse dependants in `modules.json`.
4. Preserve unrelated work and generated/corpus provenance.

## Changes

Keep commits focused and conventional. Update every affected changelog with
the behavior and migration impact. Public API changes require compatibility
evidence and documentation. Specification behavior requires a decision record,
fixture coverage, and interoperability evidence.

New direct dependencies and dependency updates must follow the
[dependency governance policy](AGENTS.md#dependencies-and-supply-chain). Package-local
update bots are forbidden; the root policy owns every module and action update.

Specification-backed changes must follow the
[specification governance contract](AGENTS.md#design), update
the affected stable decision entries, and complete the Specification Decisions
section of the pull request template. An unresolved interpretation or stale
source pin is release-blocking; peer behavior cannot silently select policy.

Selected full/release mutation gates must finish with zero surviving viable
mutants; mutation is not an ordinary local or pull-request prerequisite.

Do not add package-local workflows, permanent replacements, machine-specific
paths, bypass flags, broad mutation exclusions, or aggregate quality metrics
that hide a failing package.

## Verification

Use Go 1.27.3 for verification; the module/API minimum remains Go 1.27.0.
An isolated checkout needs no sibling repositories or coordination goal files.
Begin with deterministic tests and one active target when its ledger-decoding
boundary changes, using a disposable cache for each bounded session:

```bash
(
  set -eu
  GOCACHE="$(mktemp -d)"
  export GOCACHE
  trap 'rm -rf "${GOCACHE}"' EXIT
  GOWORK=off go test ./...
  GOWORK=off go test ./postgres -run '^$' -fuzz '^FuzzDecodeLedgerRecord$' -fuzztime=2s -timeout=2m
)
```

The deterministic command executes fuzz seeds/corpus, not active fuzzing.
It omits `integration` tests and does not qualify live PostgreSQL semantics.
Install the verified public v2.5.1 binary archive for your platform from
[go-library-tools releases](https://github.com/faustbrian/go-library-tools/releases/tag/v2.5.1),
checking `checksums.txt` against `.golib.yaml` and the archive against that
checksum set. Put the extracted `golib` binary on `PATH`; do not build Tools
from source. CI verifies the same public release while checking out its pinned
reusable workflow source.

```bash
make inventory
make check
make ci
make specification-check
golib mutation --module . --package postgres
```

Run adjacent Make commands in one disposable-cache session as shown above.
`make check` and `make ci` run the bounded contract with the declared PostgreSQL
fixture and `integration` tag; they are not Docker- or database-free. Both also
retain the cheap engine-boundary script, which rejects `database/sql`, its
driver adapters, other database drivers and the retired Goose engine in Go
imports, `go.mod`, the public API snapshot and the owned ledger.
CI requires that guard and shared CI to succeed. CI also validates repository/
cohesion structure. Specification checking is offline unless requested online.
One targeted `postgres` test operation runs once with a two-minute timeout:
contended gate and busy-session cancellation must return the context error,
and canceled release must preserve the owned connection/session. These are
deterministic gate/ownership assertions, not manufactured goroutine races or
live locking proof. Base-free checks run seeds/corpus; changed-source CI
selects causal active fuzz smoke. Optional mutation is limited to one package,
one worker and a twenty-minute deadline.

Select full qualification and release rehearsal explicitly:

```bash
make check-full
make specification-check-online
golib api check
golib release dry-run --module .
```

Full checks retain all four declared fuzz targets at 10000x/20m each, the
100ms benchmark operation, the original engine-boundary operation and all
module gates. Real integration tests retain engine conformance and
transaction/ledger assertions. The scheduled carrier uses the shared bounded
route, not an automatic full campaign. A separate CI job runs the PostgreSQL
integration suite and the `testdata/interop` v4/v5 harness on PostgreSQL 14
through 18; set `POSTGRES_VERSION` to reproduce one major locally. Online
specification checking fetches authority/errata sources when applicable.
Release rehearsal can run locally. Report unavailable or failing commands;
bounded checks alone do not establish release qualification.

## Adding A Module

Follow [repository structure policy](AGENTS.md#repository-structure). New modules
require an explicit purpose, ownership boundary, dependency review, package
catalog entry, full quality gates, documentation, changelog, license, security
policy, compatibility plan, and release dry-run.

## Source Documentation

Install the checksum-pinned public verifier declared in `.golib.yaml`.
`make source-docs-check` validates objective Go comment, marker, link and
generated-producer structure; `make check`, `make check-full` and `make ci`
require it before their existing checks. Shared CI runs the same strict
contract and includes its result in Required. This is structural enforcement,
not a substitute for source-truth review or behavioral verification.
