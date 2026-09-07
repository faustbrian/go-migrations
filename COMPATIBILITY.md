# Compatibility Policy

This repository has one releasable root Go module and follows semantic
versioning. Root releases use `v<version>` tags, such as `v1.0.0`; package
directories are not independently versioned modules and do not use
directory-prefixed tags.

The stable v1 contract is published. Patch releases MUST remain backward
compatible, and incompatible exported API or documented behavior changes
require a new major version. The latest patch in the current stable major is
the supported release for defect and security fixes.

Compatibility includes exported Go APIs, error classification, serialization,
protocol behavior, persistence schemas, environment variables, command output,
resource ownership, ordering, retry/idempotency semantics, and documented
defaults. A compile-compatible change can still be behaviorally breaking.

Specification-backed modules MUST NOT diverge from their declared standards.
Ambiguities require documented decisions and stable tests. Deprecated APIs
follow [`DEPRECATION.md`](DEPRECATION.md).
