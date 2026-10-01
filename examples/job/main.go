// Command job runs embedded migrations as a dedicated deployment task.
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"time"

	migrations "github.com/faustbrian/go-migrations/v3"
	"github.com/faustbrian/go-migrations/v3/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	connectionString := os.Getenv("DATABASE_URL")
	if connectionString == "" {
		return errors.New("DATABASE_URL is required")
	}
	database, err := sql.Open("pgx", connectionString)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = database.Close() }()

	source, err := migrations.NewFSSource(embeddedSourceFileSystem{files: migrationFiles}, "migrations")
	if err != nil {
		return fmt.Errorf("create migration source: %w", err)
	}
	backend, err := postgres.New(
		database,
		postgres.WithLockTimeout(30*time.Second),
		postgres.WithStatementTimeout(5*time.Minute),
	)
	if err != nil {
		return fmt.Errorf("create PostgreSQL backend: %w", err)
	}
	runner, err := migrations.NewRunner(source, backend, migrations.WithObserver(logObserver{}))
	if err != nil {
		return fmt.Errorf("create migration runner: %w", err)
	}

	plan, err := runner.Plan(ctx)
	if err != nil {
		return fmt.Errorf("plan migrations: %w", err)
	}
	for _, step := range plan.Steps() {
		// #nosec G706 -- action and version are numeric, and migration names are
		// constructor-validated lowercase snake case without control characters.
		log.Printf("planned action=%d version=%s name=%s", step.Action(), step.Migration().Version(), step.Migration().Name())
	}
	result, err := runner.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	// #nosec G706 -- the only formatted value is a process-local integer count.
	log.Printf("completed migrations=%d", len(result.Records()))

	status, err := runner.Status(ctx)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	for _, entry := range status.Entries() {
		// #nosec G706 -- state and version are numeric, and names are canonical
		// validated migration identifiers without control characters.
		log.Printf("status state=%d version=%s name=%s", entry.State(), entry.Version(), entry.Name())
	}

	return nil
}

type embeddedSourceFileSystem struct {
	files embed.FS
}

func (filesystem embeddedSourceFileSystem) ReadDir(
	ctx context.Context,
	root string,
	limits migrations.SourceDirectoryLimits,
) ([]migrations.SourceEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(filesystem.files, root)
	if err != nil {
		return nil, err
	}
	if len(entries) > limits.MaxEntries {
		return nil, migrations.ErrSourceLimit
	}
	converted := make([]migrations.SourceEntry, 0, len(entries))
	totalNameBytes := 0
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > limits.MaxNameBytes || len(name) > limits.MaxTotalNameBytes-totalNameBytes {
			return nil, migrations.ErrSourceLimit
		}
		totalNameBytes += len(name)
		converted = append(converted, migrations.SourceEntry{Name: name, Directory: entry.IsDir()})
	}

	return converted, ctx.Err()
}

func (filesystem embeddedSourceFileSystem) ReadFile(
	ctx context.Context,
	name string,
	maxBytes int,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contents, err := fs.ReadFile(filesystem.files, name)
	if err != nil {
		return nil, err
	}
	if len(contents) > maxBytes {
		return nil, migrations.ErrInvalidEncoding
	}

	return contents, ctx.Err()
}

type logObserver struct{}

func (logObserver) Observe(_ context.Context, event migrations.Event) {
	log.Printf(
		"migration operation=%d phase=%d version=%s duration=%s error=%v",
		event.Operation(),
		event.Phase(),
		event.Version(),
		event.Duration(),
		event.Err(),
	)
}
