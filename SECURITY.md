# Security policy

## Supported versions

The latest published `v1` and `v2` releases receive security fixes. The `v2`
line is published as `v2.0.0` at `github.com/faustbrian/go-migrations/v2`.
Supported release lines are listed here and in the compatibility documentation.

## Reporting a vulnerability

Report vulnerabilities privately through the repository's GitHub security
advisory interface. Include affected versions, impact, reproduction steps, and
any proposed mitigation. Do not open a public issue containing exploit details,
credentials, connection strings, or production schema data.

Maintainers will acknowledge a report, assess severity, prepare tests and a
coordinated fix, and publish an advisory when affected users can upgrade. See
[the security architecture](docs/security.md) for the package threat model and
deployment guidance.

The `go-migrations` maintainers own triage and coordinated remediation. Severity,
acknowledgement and remediation targets, accepted-risk handling, embargo,
advisories, reporter privacy and affected-module release procedures follow the
versioned [ecosystem vulnerability-management policy](https://github.com/faustbrian/go-library-tools/blob/37d4eea85570a6dea1ecdce2f9ec12d1aa02fbfa/docs/ecosystem/security/vulnerability-management.md).
Response targets are goals, not guarantees of a completed fix or application
rollout. An advisory identifies affected releases precisely and distinguishes
published fixes from application-owned schema and deployment changes.
