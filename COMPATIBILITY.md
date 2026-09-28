# Compatibility Policy

This repository follows semantic versioning. Root releases use `v<version>`
tags, such as the published `v1.1.0`; package directories are not independently
versioned modules and do not use directory-prefixed tags. The planned v2 module
in this source tree is not currently releasable.

The stable v1 contract is published from its original module path. This source
prepares the next v2 contract at the `/v2` module path, which remains
unpublished until a v2 tag exists. Patch releases MUST remain backward
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
