# Documentation

[Repository entry](../README.md) · [Versioned ecosystem](https://github.com/faustbrian/go-library-tools/blob/v1.4.0/docs/ecosystem/README.md)

This source contains the v3 `migrations` module for deterministic,
engine-neutral database migrations; v1 and v2 remain available. The
root package owns migration identity and orchestration,
`postgres` owns the PostgreSQL ledger and advisory-lock session,
`adapters/service` adapts an explicit runner to a one-shot service command;
`migrationsservice` preserves the released API as a compatibility facade,
and `conformance` is the backend testing helper. The caller owns the source,
observer, and database; operations accept context cancellation and the module
starts no background work. The threat model records the security controls and
accepted risks for the v3 boundary.

## Getting started

- [Migration format and ledger](migration-format.md)
- [Executable deployment-job example](../examples/job/main.go)
- [PostgreSQL integration](go-postgres.md)
- [Architecture and engine contract](architecture.md)
- [V3 API reference](https://pkg.go.dev/github.com/faustbrian/go-migrations/v3)
- [V3 backend conformance helper](https://pkg.go.dev/github.com/faustbrian/go-migrations/v3/conformance)

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

## Complete documentation map

- [Architecture and engine contract](architecture.md) — `docs/architecture.md`
- [Benchmark baselines](benchmarks.md) — `docs/benchmarks.md`
- [Compatibility](compatibility.md) — `docs/compatibility.md`
- [Replacing the execution engine](engine-replacement.md) — `docs/engine-replacement.md`
- [FAQ](faq.md) — `docs/faq.md`
- [postgres integration](go-postgres.md) — `docs/go-postgres.md`
- [Laravel-to-Go baseline runbook](laravel-baseline.md) — `docs/laravel-baseline.md`
- [Migration format and ledger](migration-format.md) — `docs/migration-format.md`
- [Operations and recovery](operations.md) — `docs/operations.md`
- [Package map](package-map.md) — `docs/package-map.md`
- [Security](security.md) — `docs/security.md`
- [Source documentation audit](source-documentation-audit.md) — `docs/source-documentation-audit.md`
- [Threat model](threat-model.md) — `docs/threat-model.md`
