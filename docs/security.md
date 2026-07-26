# Security

Migration files are trusted deployment artifacts with database-owner power.
Review them like application code, pin module dependencies, verify checksums in
CI, and restrict who can change migration and baseline files.

Use a dedicated database role with only the DDL privileges the reviewed change
requires. Protect connection strings through the platform secret mechanism and
never include SQL or credentials in observers. The built-in events omit SQL.
The role must be able to create and use `public.go_schema_migrations`; ledger
queries explicitly qualify `public` and do not trust the connection's
`search_path`.

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
| Malformed pre-existing ledger | Independent decoding and completion-state validation |
| Adapter replacement or upgrade | Neutral public API, owned ledger provenance, and compatibility corpus |
| Observer failure or sensitive SQL disclosure | Panic isolation and structured events without SQL |
| Parser resource exhaustion | A 16 MiB file limit, bounded reads, and cancellation checks |

Migration SQL itself is trusted code and can perform any operation granted to
the database role. A malicious database administrator, compromised deployment
role, PostgreSQL server compromise, and availability attacks outside configured
timeouts are out of scope. Those risks require platform access controls,
auditing, backups, and incident response rather than migration parsing.

The executable evidence and reviewed release blockers are maintained in the
[hardening evidence](hardening.md).

Report vulnerabilities privately to the maintainers. Do not open a public issue
with credentials, exploit details, or production schema data.
