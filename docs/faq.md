# FAQ

## Why not use Laravel's or Goose's table?

Those schemas are framework implementation details. Owning the ledger keeps the
public and persisted contract stable while frameworks and engines change.

## Can a migration be edited before production?

Only if it has never run in any shared environment whose ledger matters. The
safe default is always a new migration.

## Why does whitespace change the checksum?

The package hashes canonical bytes rather than attempting unsafe SQL semantic
normalization. This makes identity deterministic and auditable.

## Why did status fail on a missing old file?

Deletion makes history unverifiable. Restore the exact file; do not recreate a
similar migration with a manufactured checksum.

## Should services run migrations at startup?

No. Use a dedicated, awaited deployment Job so rollout is ordered and failures
are visible before service processes start.

## What should I do with a dirty migration?

Inspect the database and exact source, fully complete or remove its effects,
then use the explicit recovery API. Never update the ledger manually.
