# Compatibility Policy

This repository follows semantic versioning. Root releases use `v<version>`
tags, such as the published `v1.1.0`; package directories are not independently
versioned modules and do not use directory-prefixed tags. The v5 module uses
the `/v5` module suffix without a version-specific source directory.

Published releases keep their original module paths. This source contains the
v5 line; v5 drives the PostgreSQL backend with a caller-owned pgx pool instead
of `database/sql` and keeps the v3 and v4 ledger, checksum and advisory-lock
contracts.
Patch releases MUST remain backward
compatible, and incompatible exported API or documented behavior changes
require a new major version. The latest patch in each supported major is the
supported release for defect and security fixes.

Compatibility includes exported Go APIs, error classification, serialization,
protocol behavior, persistence schemas, environment variables, command output,
resource ownership, ordering, retry/idempotency semantics, and documented
defaults. A compile-compatible change can still be behaviorally breaking.

Specification-backed modules MUST NOT diverge from their declared standards.
Ambiguities require documented decisions and stable tests. API replacement
follows [`DEPRECATION.md`](DEPRECATION.md).
