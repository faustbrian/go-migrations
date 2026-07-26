// Command job runs embedded migrations as a dedicated deployment task.
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	migrations "github.com/faustbrian/golib/pkg/migrations"
	"github.com/faustbrian/golib/pkg/migrations/postgres"
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

	source, err := migrations.NewFSSource(migrationFiles, "migrations")
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
		log.Printf("planned action=%d version=%s name=%s", step.Action(), step.Migration().Version(), step.Migration().Name())
	}
	result, err := runner.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	log.Printf("completed migrations=%d", len(result.Records()))

	status, err := runner.Status(ctx)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	for _, entry := range status.Entries() {
		log.Printf("status state=%d version=%s name=%s", entry.State(), entry.Version(), entry.Name())
	}

	return nil
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
