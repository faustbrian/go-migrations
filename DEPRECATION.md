# API Replacement Policy

A breaking replacement removes the superseded API in the new major version.
The release notes and [compatibility guide](docs/compatibility.md) must identify the
replacement, reason, observable behavior changes and consumer adoption steps.
Do not introduce an alias, forwarding package, `Deprecated:` delegate or
parallel engine to postpone migration. Published versions and their history
remain immutable; they are not part of the current supported source surface.

Security or correctness defects may require a faster release; the release
notes must explain the change. Silent behavior changes are prohibited.
Replacements are checked during compatibility and release review.
