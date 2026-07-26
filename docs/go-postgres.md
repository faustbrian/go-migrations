# postgres integration

`migrations` and `postgres` must remain sibling dependencies. The service
composition root obtains the underlying `*sql.DB` from its `postgres`
connection component and passes that standard-library handle to this package.
Neither library imports the other.

Until `postgres` publishes a stable database-handle interface, keep the
adapter service-local:

```go
type SQLDatabaseProvider interface {
	SQLDB() *sql.DB
}

func migrationRunner(
	provider SQLDatabaseProvider,
	files fs.FS,
) (*migrations.Runner, error) {
	source, err := migrations.NewFSSource(files, "migrations")
	if err != nil {
		return nil, err
	}
	backend, err := postgres.New(provider.SQLDB())
	if err != nil {
		return nil, err
	}

	return migrations.NewRunner(source, backend)
}
```

Adapt the method name to the service's actual `postgres` wrapper; do not add
that wrapper type to this module's public API. The migration backend does not
take ownership of the `*sql.DB`. The job closes the higher-level connection
component after the runner completes.

Do not pass an application transaction to the runner. The PostgreSQL backend
owns transaction boundaries so schema SQL and ledger state remain atomic. Keep
`sqlc` generation in the consuming service's build and CI; generated queries,
models, and schema inputs are not migration runtime state.
