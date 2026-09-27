# Documentation

This source prepares the next v2 `migrations` module for deterministic,
engine-neutral database migrations; v1 remains the latest published line. The
root package owns migration identity and orchestration,
`postgres` owns the PostgreSQL ledger and advisory-lock session,
`adapters/service` adapts an explicit runner to a one-shot service command;
`migrationsservice` preserves the released API as a compatibility facade,
and `conformance` is the backend testing helper. The caller owns the source,
observer, and database; operations accept context cancellation and the module
starts no background work. The threat model records the security controls and
accepted risks for the planned v2 boundary.

## Getting started

- [Migration format and ledger](migration-format.md)
- [Executable deployment-job example](../examples/job/main.go)
- [PostgreSQL integration](go-postgres.md)
- [Architecture and engine contract](architecture.md)
- [Published v1 API reference](https://pkg.go.dev/github.com/faustbrian/go-migrations)
- [Published v1 backend conformance testing helper](https://pkg.go.dev/github.com/faustbrian/go-migrations/conformance)

## Operations

- [Operations and disaster recovery](operations.md)
- [Laravel-to-Go baseline runbook](laravel-baseline.md)
- [Security](security.md)
- [Versioned threat model](threat-model.md)
- [Compatibility](compatibility.md)
- [FAQ and troubleshooting](faq.md)

## Maintainers

- [Benchmark methodology and baselines](benchmarks.md)
- [Replacing the execution engine](engine-replacement.md)
- [Contributing](../CONTRIBUTING.md)
- [Release history](../CHANGELOG.md)
- [Compatibility policy](../COMPATIBILITY.md)
- [Support](../SUPPORT.md)
- [Private security reporting](../SECURITY.md)
- [License](../LICENSE)

For ecosystem-wide ownership and selection guidance, see the versioned
[Golib ecosystem index](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/README.md)
and its [Persistence and durability family](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/design-language.md#package-families-and-selection).
