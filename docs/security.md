# Security

The repository-specific, versioned [threat model](threat-model.md) inventories
assets, trust boundaries, controls, accepted risks, and v2 release
requirements. This page provides the corresponding operator guidance.

Migration files are trusted deployment artifacts with database-owner power.
Review them like application code, pin module dependencies, verify checksums in
CI, and restrict who can change migration and baseline files.

Use a dedicated database role with only the DDL privileges the reviewed change
requires. Protect connection strings through the platform secret mechanism and
never include SQL or credentials in observers. The built-in events omit SQL
and render database failures through stable redacted categories.
Observers are trusted synchronous callbacks: keep their work bounded and
nonblocking, or use an application-owned bounded nonblocking handoff. A blocked
observer can delay migration completion and advisory-lock release; the runner
cannot forcibly interrupt it. Cleanup events use a context detached from caller
cancellation, so observer implementations need their own finite work bound.
The role must be able to create and use `public.migrations`; ledger
queries explicitly qualify `public` and do not trust the connection's
`search_path`. Automatic legacy-ledger adoption also requires ownership of
the old table (or membership in its owning role) and CREATE on `public`.

The parser rejects ambiguous filenames, directives, encodings, unrelated
entries, and oversized files. The planner fails closed on history divergence.
Advisory locks prevent concurrent package jobs but do not prevent unrelated
administrators or frameworks from changing schema; deployment controls remain
required during baseline review and migration windows.

## Threat review

| Threat | Mitigation |
| --- | --- |
| Modified or repackaged migration | Canonical SHA-256 identity and complete-history validation |
| Duplicate, deleted, renamed, reordered, or gapped history | Parser, planner, and status fail closed before execution |
| Hostile `search_path` | Every owned-ledger query explicitly uses `public` |
| Concurrent or restarted deployment jobs | Connection-bound advisory lock and post-lock revalidation |
| Process or connection loss | Atomic rollback or a persisted dirty row requiring explicit recovery |
| Baseline against partial, drifted, or advanced schema | Serializable exact schema fingerprint comparison |
| Malformed or out-of-range ledger values | Constructor range checks plus independent version, duration, and completion-state validation before rollback or recovery mutation |
| Adapter replacement or upgrade | Neutral public API, owned ledger provenance, and compatibility corpus |
| Observer failure or sensitive SQL disclosure | Panic isolation and structured events without SQL |
| Parser resource exhaustion | A cancellation-aware provider contract plus 4 KiB source-root, 4,096-file, 255-byte canonical-name, 1 MiB aggregate filename, and 16 MiB file-content limits that are enforced by the provider and revalidated by the package |
| Oversized ledger or schema catalog | 4,097 ledger records and 16 MiB ledger text; 10,000 schema objects, bounded fields, and 16 MiB schema text |
| Missing caller deadline during source or database work | Finite default source, lock, statement, and primary-operation timeouts, with only positive finite overrides |

Migration SQL itself is trusted code and can perform any operation granted to
the database role. A malicious database administrator, compromised deployment
role, and PostgreSQL server compromise are out of scope. Deliberate resource
exhaustion within caller-selected finite timeout budgets remains a deployment
capacity concern. Those risks require platform access controls, auditing,
backups, and incident response rather than migration parsing.

No known unowned Critical or High finding remains in the v2 source.
The standard-library transaction and cleanup cancellation limitation and trusted
observer callback boundary are owned explicitly in the threat model. V2.0.0 is
published; each application must separately validate its migration and deployment.

Operational controls and compatibility constraints are documented in the
[operations guide](operations.md) and [compatibility policy](compatibility.md).

Report vulnerabilities privately to the maintainers. Do not open a public issue
with credentials, exploit details, or production schema data.
