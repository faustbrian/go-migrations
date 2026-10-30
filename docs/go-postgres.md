# postgres integration

[Documentation index](README.md) · [Repository entry](../README.md)

`migrations` and `postgres` must remain sibling dependencies. The service
composition root obtains the underlying `*pgxpool.Pool` from its `postgres`
connection component and passes that pgx pool to this package. Neither library
imports the other; both depend only on `github.com/jackc/pgx/v5`.

Until `postgres` publishes a stable pool-handle interface, keep the adapter
service-local:

```go
type PoolProvider interface {
	Pool() *pgxpool.Pool
}

func migrationRunner(
	provider PoolProvider,
	files migrations.SourceFileSystem,
) (*migrations.Runner, error) {
	source, err := migrations.NewFSSource(files, "migrations")
	if err != nil {
		return nil, err
	}
	backend, err := postgres.New(provider.Pool())
	if err != nil {
		return nil, err
	}

	return migrations.NewRunner(source, backend)
}
```

Adapt the method name to the service's actual `postgres` wrapper; do not add
that wrapper type to this module's public API. The migration backend does not
take ownership of the `*pgxpool.Pool`. The job closes the higher-level
connection component after the runner completes. Each migration session holds
one pooled connection until release, so the pool must allow it alongside any
other work the job runs at the same time.

Do not pass an application transaction to the runner. The PostgreSQL backend
owns transaction boundaries so schema SQL and ledger state remain atomic. Keep
`sqlc` generation in the consuming service's build and CI; generated queries,
models, and schema inputs are not migration runtime state.

Choose the next guide in the [documentation index](README.md)
or return to the [repository entry](../README.md) for installation and ownership.
