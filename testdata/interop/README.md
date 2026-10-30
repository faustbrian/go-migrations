# v4/v5 interoperability harness

This non-releasable module proves that a v5 runner can take over a
PostgreSQL database that the published `go-migrations/v4` v4.0.0 wrote. It
writes a baseline, a clean row, a dirty row, a namespaced ledger and a
partially executed command through v4, then requires v5 to read, plan,
recover, append and resume that history. It also checks that v4 and v5
runners contend for the same advisory lock.

The tests need Docker and run with the `integration` tag:

```sh
cd testdata/interop && GOWORK=off go test -tags integration ./...
```

The `replace` directive points at this source tree because v5 is not yet
published. The module imports `database/sql` only to drive v4; the
repository engine-boundary check excludes it for that reason.
