# Source documentation audit

## Scope and acceptance

Base: `291cfe3b38dcd4796bc5c23aba7565f156ffcea7` (remote main).
Tier A: clarify actual contracts without changing runtime, SQL, configuration,
dependencies, historical API/fixture evidence or security-review history. Owner:
this repository. Direct consumers are root callers, PostgreSQL backend callers,
service adapters and backend implementers. Risks: source/lock snapshot confusion,
partial durable outcomes, operator recovery assertions, incomplete schema
fingerprints, borrowed dependencies, diagnostics and uninterruptible cleanup.
Smallest checks: all-file parse/noncomment token and AST equality, public comment
and canonical header inspection, rendered docs, native API regeneration,
engine-boundary guard, local documentation links, changed-content secrets and
one ordinary complete final-diff inspection. No database qualification claim.

## Reproducible inventory

Use `git ls-files '*.go' 'go.mod' 'go.sum'`, `cat modules.json packages.json`,
`rg -n '^func (Test|Fuzz|Benchmark|Example)|go:|Code generated|TODO|FIXME|HACK|XXX|nolint|nosec|Deprecated:' --glob '*.go'`,
and inspect every listed file, including bodies/assertions and helper ownership.
One module: `github.com/faustbrian/go-migrations/v3`; seven package directories;
48 tracked Go files. Public types, constants, errors, fields, methods and
interface members are included, not just top-level function comments.

| Package directory | Investigated contracts and evidence |
| --- | --- |
| `.` | identity, source, plans/status, runner, baseline/recovery; constructor, options, divergence, failure and compatibility assertions |
| `adapters/service` | canonical one-shot command and callback/component lifecycle assertions |
| `migrationsservice` | deprecated named-type facade, error translation and executable example |
| `conformance` | backend-neutral supplied-harness assertions and immutable in-memory source helper |
| `postgres` | lock/session gate, ledger SQL, schema query/framing, bounded contexts, dirty/transaction cleanup; SQL mocks, driver fakes, tagged database/process tests |
| `internal/goose` | hidden engine, transaction/leased-connection execution and redacted SQL causes |
| `examples/job` | embedded deployment job; caller-owned database, source and synchronous observer |

Every tracked Go file is accounted for below. Existing test names and assertions
remain evidence, not fresh execution or a claim that every database boundary
has been qualified in this pass.

| File | Role |
| --- | --- |
| `adapter_successor_test.go` | assertions and test-only helpers |
| `adapters/service/service.go` | implementation/entry point |
| `adapters/service/service_test.go` | assertions and test-only helpers |
| `baseline.go` | implementation/entry point |
| `baseline_test.go` | assertions and test-only helpers |
| `compatibility_test.go` | assertions and test-only helpers |
| `conformance/suite.go` | implementation/entry point |
| `contract_internal_test.go` | assertions and test-only helpers |
| `examples/job/main.go` | implementation/entry point |
| `internal/goose/adapter.go` | implementation/entry point |
| `internal/goose/adapter_internal_test.go` | assertions and test-only helpers |
| `internal/goose/adapter_test.go` | assertions and test-only helpers |
| `migration.go` | implementation/entry point |
| `migration_test.go` | assertions and test-only helpers |
| `migrationsservice/example_test.go` | runnable example (unchanged) |
| `migrationsservice/migrationsservice.go` | implementation/entry point |
| `migrationsservice/migrationsservice_internal_test.go` | assertions and test-only helpers |
| `migrationsservice/migrationsservice_test.go` | assertions and test-only helpers |
| `mutation_boundaries_internal_test.go` | assertions and test-only helpers |
| `mutation_survivors_internal_test.go` | assertions and test-only helpers |
| `options_characterization_internal_test.go` | assertions and test-only helpers |
| `planner.go` | implementation/entry point |
| `planner_benchmark_test.go` | workload fixtures and sinks, no performance claim |
| `planner_test.go` | assertions and test-only helpers |
| `postgres/backend.go` | implementation/entry point |
| `postgres/backend_fuzz_test.go` | seed corpus and invariant oracle |
| `postgres/backend_test.go` | assertions and test-only helpers |
| `postgres/fault_test.go` | assertions and test-only helpers |
| `postgres/integration_test.go` | integration-tag database/process assertions (not run) |
| `postgres/mutation_contract_internal_test.go` | assertions and test-only helpers |
| `postgres/options_characterization_internal_test.go` | assertions and test-only helpers |
| `postgres/schema.go` | implementation/entry point |
| `postgres/schema_benchmark_test.go` | workload fixtures and sinks, no performance claim |
| `postgres/session_state_regression_internal_test.go` | assertions and test-only helpers |
| `recovery.go` | implementation/entry point |
| `recovery_test.go` | assertions and test-only helpers |
| `runner.go` | implementation/entry point |
| `runner_failure_internal_test.go` | assertions and test-only helpers |
| `runner_test.go` | assertions and test-only helpers |
| `service_dependency_test.go` | assertions and test-only helpers |
| `source.go` | implementation/entry point |
| `source_benchmark_test.go` | workload fixtures and sinks, no performance claim |
| `source_diagnostics_regression_internal_test.go` | assertions and test-only helpers |
| `source_fuzz_test.go` | seed corpus and invariant oracle |
| `source_internal_test.go` | assertions and test-only helpers |
| `source_test.go` | assertions and test-only helpers |
| `status.go` | implementation/entry point |
| `status_test.go` | assertions and test-only helpers |

## Missing contracts versus investigated questions

Repair missing Session/SourceFileSystem/Observer methods, adapter option/error
fields, schema-object fields and Harness inputs. Expand the seven canonical
package/command comments. Repair inaccurate source/lock race elimination,
dirty timestamps, automatic recovery certification, comprehensive schema
fingerprint and universal error-redaction wording. Keep useful private error
and state-gate comments where they explain observable causes or cleanup.

Investigated: SHA-256 migration framing is `go-migrations/v1`, SQL bytes are not
normalized or authenticated; schema framing is
`go-migrations/postgres-schema/v1`. Fingerprints include only the selected
catalog query, not grants/ownership, RLS enabled/forced flags, policy role
targets, row contents or all PostgreSQL properties. Recovery does not execute
or inspect migration SQL effects. No batch transaction or automatic retry is
provided. Cleanup errors can follow committed work, and commit failures may
have uncertain outcomes. Synchronous observers can block; dependencies and
context cooperation remain caller/backend obligations. Nil-interface checks
do not establish that an interface holding a nil pointer is usable.

Not a missing comment: test-local helpers need only non-obvious ownership or
oracle rationale, not exported-name filler. No new specification record is
needed: the module declares no specifications; the versioned owned formats,
compatibility fixtures, code and named tests are authoritative here.

Unresolved/runtime follow-up, not repaired: NewRecovery accepts nonzero versions
above the signed ledger range (Runner subsequently requires matching source);
a Down directive with no bytes after it is treated as an absent rollback,
while whitespace-only bytes are rejected. No new characterization test or
behavior fix is included. Operators must separately review unrepresented
schema/security properties and database/driver finalization outcomes.

## Variants, generators and evidence boundaries

Only `postgres/integration_test.go` has `integration` build constraint. No
tracked Go file is generated and no go:generate directive exists. The job's
`go:embed migrations/*.sql`, three G706 and three G115 rationale annotations,
two deprecated-import staticcheck exceptions and Deprecated header are retained.
No TODO/FIXME/HACK/XXX debt marker needs invented resolution. Four fuzz targets
are FSSource, migration identity, checksum parsing and ledger decoding; checksum
saved corpus is preserved. Five benchmark entry points cover parsing, loading,
planning, status and schema fingerprinting. The deprecated service ExampleNew
and real-database job are inspected, not executed unchanged. Integration tests
cover supported scenarios, process death, lock loss, single-connection pools,
Laravel fixtures and aligned v1 history; none is newly certified here.

Discover native generation in both `.golib.yaml` and `scripts/` plus
`verification/package.mk`: selected `api/v3.txt` is go-doc, packages in order
`.`, `./adapters/service`, `./conformance`, `./migrationsservice`, `./postgres`.
Concatenate `go doc -all` stdout without separators, trim trailing newline bytes
and append one newline; regenerate twice for stability. Preserve historical
`api/v1.txt` and `api/v2.txt`. The engine-boundary script rejects the hidden
engine name in public API prose as well as public imports/ledger provenance.
Pin repository Go 1.27.2, GOWORK=off and all three disposable caches for the
serialized bounded session. Native source-docs and local docs checks are scoped
here; no service, race, fuzz campaign, benchmark, mutation, consumer, release or
online reference check is warranted by unchanged runtime/immutable inputs.
Historical mutation archives, source pins, SQL fixtures and threat-model review
are preserved. Runtime proof is unchanged; local documentation proof does not
assert hosted CI, publication or PostgreSQL qualification.

## Bounded local validation

Go 1.27.2 formatted the 17 changed Go files. All 48 tracked executable token
streams (including implicit semicolons/literals) and position/comment-free ASTs
match the base exactly; build/embed/lint/security/deprecation markers match.
All 265 applicable exported declarations/fields/interface methods have name-led
contracts; seven canonical package/command headers and 312 comment groups
parsed/rendered. All seven package go-doc views rendered. The five configured
views produced identical native api/v3.txt bytes in two complete generations.
Historical API snapshots, module/catalog/policy, SQL/compatibility/fuzz fixtures,
generator/engine scripts and dated threat review retain their original bytes.

The engine-boundary script, native source-docs check, native docs check --local
and diff whitespace check passed. No example executable changed, so ExampleNew
and the database job were not run. Database integration/qualification, runtime
unit suites, race/leak, active fuzz, benchmarks, mutation, consumer, release and
online checks were deliberately not run for this Tier A pass. All three exact
owned Go caches, helper and rendered copies were removed; historical evidence
was retained. Final changed-content secret and complete-diff inspection belong
to the local commit handoff, not database or hosted-CI proof.

## Objective Enforcement

The checksum-pinned public Tools v2.4.0 verifier now supplies
`make source-docs-check` as a prerequisite of the existing local and CI Make
routes. Ordinary shared CI selects strict source-documentation checking and
requires its result. Native prerequisites, operation budgets, security and
release controls are retained. Hosted exact-head evidence is recorded by the
coordination ledger; configuration alone does not establish green delivery.
