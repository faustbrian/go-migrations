# Migration format and ledger

## Canonical SQL files

Files live in one `fs.FS` directory and use
`<positive-version>_<snake_case_name>.sql`. The directory must contain only
migration files. Versions are numeric identities; zero, duplicates, invalid
UTF-8, NUL bytes, byte-order marks, files over 16 MiB, and unknown directives
are rejected. `NewMigration` applies the same UTF-8, NUL, and aggregate SQL size
rules when applications construct a migration without `FSSource`. Versions
must fit a positive signed 64-bit integer so every accepted identity is
representable in the owned PostgreSQL `bigint` ledger.

```sql
-- +migrations Up
CREATE TABLE widgets (id bigint PRIMARY KEY);

-- +migrations Down
DROP TABLE widgets;
```

`Up` is required and must contain SQL. `Down` is optional; omitting it makes the
migration irreversible, while a present `Down` section must contain
non-whitespace SQL. Directives must occupy an entire line. Content before `Up`
must be whitespace.

For PostgreSQL statements that cannot run in a transaction:

```sql
-- +migrations NoTransaction
-- +migrations Up
CREATE INDEX CONCURRENTLY widgets_name_idx ON widgets (name);
-- +migrations Down
DROP INDEX CONCURRENTLY widgets_name_idx;
```

`NoTransaction` must appear once, before `Up`. A failure leaves the ledger row
dirty because SQL may have partially completed. Keep each no-transaction
direction to one PostgreSQL command. Drivers may send a multi-command string as
one implicit transaction, which defeats commands such as `CREATE INDEX
CONCURRENTLY`; use several ordered migration files instead.

## Identity and immutability

The SHA-256 checksum covers the format version, numeric version, name,
transaction mode, up SQL, and down SQL. SQL sections use byte-length prefixes,
so directive-like text inside SQL cannot shift the identity boundary. Every
byte matters, including comments and whitespace. Never edit a migration after
it has run; add a new migration.

## Owned ledger

`public.go_schema_migrations` contains one row per Go migration or reviewed
baseline. Every ledger operation explicitly qualifies the `public` schema;
the connection's `search_path` cannot redirect migration history:

| Column | Meaning |
| --- | --- |
| `version` | Positive immutable identity and primary key |
| `kind` | `migration` or `baseline` |
| `name` | Canonical name |
| `checksum` | Lowercase `sha256:` identity |
| `started_at` | Attempt start time |
| `finished_at` | Completion time; null while dirty |
| `execution_time_ms` | Persisted elapsed milliseconds |
| `dirty` | Explicit unresolved outcome |
| `engine` / `engine_version` | Owned backend contract provenance |

The package owns this schema. Applications must not write it directly. The
Laravel `migrations` table and Goose tables are unrelated and untouched.
Checksums are lowercase algorithm-qualified SHA-256 values; the all-zero value
is reserved as the invalid/uninitialized sentinel and is rejected when parsed.
Ledger reads also reject any dirty row with a completion time or clean row
without one, independently of database constraints.
Migration rows persist `postgres` / `v1`, not the replaceable adapter name or
version. Adapter upgrades therefore do not rewrite or redefine ledger history.
