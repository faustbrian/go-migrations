//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	migrations "github.com/faustbrian/golib/pkg/migrations"
	"github.com/faustbrian/golib/pkg/migrations/conformance"
	migrationpostgres "github.com/faustbrian/golib/pkg/migrations/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestPostgresEngineConformance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	version := os.Getenv("POSTGRES_VERSION")
	if version == "" {
		version = "18"
	}
	container, err := tcpostgres.Run(
		ctx,
		"postgres:"+version+"-alpine",
		tcpostgres.WithDatabase("migrations_test"),
		tcpostgres.WithUsername("migrations"),
		tcpostgres.WithPassword("migrations"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL %s: %v", version, err)
	}
	testcontainers.CleanupContainer(t, container)
	connectionString, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	admin := openDatabase(t, connectionString)
	var conformanceDatabases sync.Map
	conformance.Run(t, conformance.Harness{
		NewRunner: func(t *testing.T, files fstest.MapFS) *migrations.Runner {
			t.Helper()
			value, ok := conformanceDatabases.Load(t.Name())
			if !ok {
				parts := strings.Split(t.Name(), "/")
				database := isolatedDatabase(
					t,
					admin,
					connectionString,
					"conformance_"+strings.ReplaceAll(parts[len(parts)-1], " ", "_"),
				)
				conformanceDatabases.Store(t.Name(), database)
				value = database
			}

			return newIntegrationRunner(t, value.(*sql.DB), files)
		},
		Exec: func(t *testing.T, statement string) {
			t.Helper()
			value, ok := conformanceDatabases.Load(t.Name())
			if !ok {
				t.Fatal("conformance database not initialized")
			}
			if _, err := value.(*sql.DB).ExecContext(context.Background(), statement); err != nil {
				t.Fatalf("execute conformance cleanup: %v", err)
			}
		},
		TransactionalUp:      "CREATE TABLE conformance_transactional (id bigint);",
		TransactionalDown:    "DROP TABLE conformance_transactional;",
		NoTransactionUp:      "CREATE INDEX CONCURRENTLY conformance_transactional_id_idx ON conformance_transactional (id);",
		NoTransactionDown:    "DROP INDEX CONCURRENTLY conformance_transactional_id_idx;",
		TransactionalFailure: "CREATE TABLE conformance_rolled_back (id bigint); SELECT missing_column;",
		NoTransactionFailure: "CREATE TABLE conformance_partial (id bigint); SELECT missing_column;",
		RemovePartialEffects: "DROP TABLE IF EXISTS conformance_partial;",
	})

	t.Run("apply status and rollback", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "apply")
		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000001_create_users.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\n" +
					"CREATE TABLE users (id bigint PRIMARY KEY, email text);\n" +
					"-- +migrations Down\n" +
					"DROP TABLE users;\n",
			)},
			"migrations/000002_index_users.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations NoTransaction\n" +
					"-- +migrations Up\n" +
					"CREATE INDEX CONCURRENTLY users_email_idx ON users (email);\n" +
					"-- +migrations Down\n" +
					"DROP INDEX CONCURRENTLY users_email_idx;\n",
			)},
		})

		result, err := runner.Up(context.Background())
		if err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		if len(result.Records()) != 2 {
			t.Fatalf("Up() records = %d, want 2", len(result.Records()))
		}
		status, err := runner.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		for _, entry := range status.Entries() {
			if entry.State() != migrations.StateApplied {
				t.Fatalf("Status() entry = %#v, want applied", entry)
			}
		}
		if _, err := runner.Down(context.Background(), 2); err != nil {
			t.Fatalf("Down() error = %v", err)
		}
		var ledgerCount int
		if err := database.QueryRowContext(context.Background(), "SELECT count(*) FROM public.go_schema_migrations").Scan(&ledgerCount); err != nil {
			t.Fatalf("count ledger: %v", err)
		}
		if ledgerCount != 0 {
			t.Fatalf("ledger count = %d, want 0", ledgerCount)
		}
	})

	t.Run("concurrent runners serialize", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "concurrent")
		files := fstest.MapFS{
			"migrations/000001_create_once.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nSELECT pg_sleep(0.2);\nCREATE TABLE created_once (id bigint);\n",
			)},
		}
		first := newIntegrationRunner(t, database, files)
		second := newIntegrationRunner(t, database, files)
		results := make(chan migrations.Result, 2)
		errorsFound := make(chan error, 2)
		var wait sync.WaitGroup
		for _, runner := range []*migrations.Runner{first, second} {
			wait.Add(1)
			go func(runner *migrations.Runner) {
				defer wait.Done()
				result, runErr := runner.Up(context.Background())
				results <- result
				errorsFound <- runErr
			}(runner)
		}
		wait.Wait()
		close(results)
		close(errorsFound)
		for runErr := range errorsFound {
			if runErr != nil {
				t.Fatalf("concurrent Up() error = %v", runErr)
			}
		}
		completed := 0
		for result := range results {
			completed += len(result.Records())
		}
		if completed != 1 {
			t.Fatalf("completed migrations = %d, want exactly 1", completed)
		}
	})

	t.Run("single connection pool prepares under lock", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "single_connection")
		database.SetMaxOpenConns(1)
		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000001_single_connection.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nCREATE TABLE single_connection (id bigint);\n",
			)},
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result, err := runner.Up(ctx)
		if err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		if len(result.Records()) != 1 {
			t.Fatalf("Up() records = %d, want 1", len(result.Records()))
		}
	})

	t.Run("ledger ignores hostile search path", func(t *testing.T) {
		setupDatabase := isolatedDatabase(t, admin, connectionString, "search_path")
		if _, err := setupDatabase.ExecContext(context.Background(), "CREATE SCHEMA shadow"); err != nil {
			t.Fatalf("create shadow schema: %v", err)
		}
		connectionURL, err := url.Parse(databaseURL(t, connectionString, "test_search_path"))
		if err != nil {
			t.Fatalf("parse search-path connection string: %v", err)
		}
		query := connectionURL.Query()
		query.Set("search_path", "shadow,public")
		connectionURL.RawQuery = query.Encode()
		database := openDatabase(t, connectionURL.String())
		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000001_search_path.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nCREATE TABLE search_path_table (id bigint);\n",
			)},
		})
		if _, err := runner.Up(context.Background()); err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		var publicLedger bool
		var shadowLedger bool
		if err := database.QueryRowContext(
			context.Background(),
			"SELECT to_regclass('public.go_schema_migrations') IS NOT NULL, to_regclass('shadow.go_schema_migrations') IS NOT NULL",
		).Scan(&publicLedger, &shadowLedger); err != nil {
			t.Fatalf("inspect ledger schemas: %v", err)
		}
		if !publicLedger || shadowLedger {
			t.Fatalf("ledger schemas public=%t shadow=%t", publicLedger, shadowLedger)
		}
	})

	t.Run("historical v1 ledger upgrades without rewriting history", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "historical_ledger")
		ledgerSQL, err := os.ReadFile("../testdata/compatibility/v1/ledger.sql")
		if err != nil {
			t.Fatalf("read historical ledger fixture: %v", err)
		}
		if _, err := database.ExecContext(context.Background(), string(ledgerSQL)); err != nil {
			t.Fatalf("install historical ledger fixture: %v", err)
		}
		source, err := migrations.NewFSSource(
			os.DirFS("../testdata/compatibility/v1"),
			"migrations",
		)
		if err != nil {
			t.Fatalf("NewFSSource() error = %v", err)
		}
		backend, err := migrationpostgres.New(database)
		if err != nil {
			t.Fatalf("postgres.New() error = %v", err)
		}
		runner, err := migrations.NewRunner(source, backend)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}
		status, err := runner.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		entries := status.Entries()
		if len(entries) != 2 ||
			entries[0].State() != migrations.StateApplied ||
			entries[1].State() != migrations.StatePending {
			t.Fatalf("historical Status() = %#v", entries)
		}
		result, err := runner.Up(context.Background())
		if err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		if records := result.Records(); len(records) != 1 || records[0].Version() != 2 {
			t.Fatalf("Up() records = %#v", records)
		}
		rows, err := database.QueryContext(
			context.Background(),
			"SELECT version, engine, engine_version FROM public.go_schema_migrations ORDER BY version",
		)
		if err != nil {
			t.Fatalf("query upgraded ledger: %v", err)
		}
		defer func() { _ = rows.Close() }()
		versions := make([]int64, 0, 2)
		backends := make([]string, 0, 2)
		engines := make([]string, 0, 2)
		for rows.Next() {
			var version int64
			var backend string
			var engine string
			if err := rows.Scan(&version, &backend, &engine); err != nil {
				t.Fatalf("scan upgraded ledger: %v", err)
			}
			versions = append(versions, version)
			backends = append(backends, backend)
			engines = append(engines, engine)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate upgraded ledger: %v", err)
		}
		if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 ||
			backends[0] != "postgres" || backends[1] != "postgres" ||
			engines[0] != "v1" || engines[1] != "v1" {
			t.Fatalf(
				"upgraded ledger versions=%v backends=%v contracts=%v",
				versions,
				backends,
				engines,
			)
		}
	})

	t.Run("concurrent processes serialize", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "process_concurrent")
		childURL := databaseURL(t, connectionString, "test_process_concurrent")
		commands := []*exec.Cmd{
			migrationHelperCommand(childURL, "concurrent"),
			migrationHelperCommand(childURL, "concurrent"),
		}
		outputs := make([]bytes.Buffer, len(commands))
		for index, command := range commands {
			command.Stdout = &outputs[index]
			command.Stderr = &outputs[index]
			if err := command.Start(); err != nil {
				t.Fatalf("start migration process %d: %v", index, err)
			}
		}
		for index, command := range commands {
			if err := command.Wait(); err != nil {
				t.Fatalf("migration process %d: %v\n%s", index, err, outputs[index].String())
			}
		}
		var count int
		if err := database.QueryRowContext(
			context.Background(),
			"SELECT count(*) FROM public.go_schema_migrations",
		).Scan(&count); err != nil {
			t.Fatalf("count process ledger: %v", err)
		}
		if count != 1 {
			t.Fatalf("process ledger count = %d, want 1", count)
		}
	})

	t.Run("terminated lock waiter cannot prepare or mutate ledger", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "lock_wait_crash")
		backend, err := migrationpostgres.New(database)
		if err != nil {
			t.Fatalf("postgres.New() error = %v", err)
		}
		owner, err := backend.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire owner session: %v", err)
		}
		childURL := databaseURL(t, connectionString, "test_lock_wait_crash")
		command := migrationHelperCommand(childURL, "concurrent")
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if err := command.Start(); err != nil {
			t.Fatalf("start lock waiter: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		var ledgerExists bool
		if err := database.QueryRowContext(
			context.Background(),
			"SELECT to_regclass('public.go_schema_migrations') IS NOT NULL",
		).Scan(&ledgerExists); err != nil {
			t.Fatalf("inspect waiter ledger: %v", err)
		}
		if ledgerExists {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("lock waiter prepared ledger without owning advisory lock")
		}
		if err := command.Process.Kill(); err != nil {
			t.Fatalf("kill lock waiter: %v", err)
		}
		if err := command.Wait(); err == nil {
			t.Fatal("killed lock waiter exited successfully")
		}
		if err := owner.Release(context.Background()); err != nil {
			t.Fatalf("release owner session: %v", err)
		}
		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000001_process.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nCREATE TABLE process_once (id bigint);\n",
			)},
		})
		result, err := runner.Up(context.Background())
		if err != nil {
			t.Fatalf("retry after waiter termination: %v", err)
		}
		if len(result.Records()) != 1 {
			t.Fatalf("retry records = %d, want 1", len(result.Records()))
		}
	})

	t.Run("real lock timeout and cancellation preserve owner", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "lock_contexts")
		ownerBackend, err := migrationpostgres.New(database)
		if err != nil {
			t.Fatalf("postgres.New(owner) error = %v", err)
		}
		owner, err := ownerBackend.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire(owner) error = %v", err)
		}
		contenderBackend, err := migrationpostgres.New(
			database,
			migrationpostgres.WithLockRetryInterval(10*time.Millisecond),
			migrationpostgres.WithLockTimeout(100*time.Millisecond),
		)
		if err != nil {
			t.Fatalf("postgres.New(contender) error = %v", err)
		}
		if _, err := contenderBackend.Acquire(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Acquire(timeout) error = %v, want deadline", err)
		}
		cancelContext, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := contenderBackend.Acquire(cancelContext); !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire(canceled) error = %v, want canceled", err)
		}
		if err := owner.Prepare(context.Background()); err != nil {
			t.Fatalf("owner lost lock after contenders: %v", err)
		}
		if err := owner.Release(context.Background()); err != nil {
			t.Fatalf("Release(owner) error = %v", err)
		}
	})

	t.Run("connection loss releases advisory ownership for retry", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "connection_loss")
		backend, err := migrationpostgres.New(database)
		if err != nil {
			t.Fatalf("postgres.New() error = %v", err)
		}
		session, err := backend.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire() error = %v", err)
		}
		var terminated bool
		if err := database.QueryRowContext(context.Background(), `
SELECT pg_terminate_backend(activity.pid)
FROM pg_locks locks
JOIN pg_stat_activity activity ON activity.pid = locks.pid
WHERE locks.locktype = 'advisory'
  AND locks.granted
  AND activity.datname = current_database()
LIMIT 1
`).Scan(&terminated); err != nil {
			t.Fatalf("terminate lock connection: %v", err)
		}
		if !terminated {
			t.Fatal("pg_terminate_backend() = false")
		}
		if err := session.Release(context.Background()); err == nil {
			t.Fatal("Release() error = nil after connection termination")
		}
		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000001_after_connection_loss.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nCREATE TABLE after_connection_loss (id bigint);\n",
			)},
		})
		result, err := runner.Up(context.Background())
		if err != nil {
			t.Fatalf("Up() after connection loss: %v", err)
		}
		if len(result.Records()) != 1 {
			t.Fatalf("Up() records = %d, want 1", len(result.Records()))
		}
	})

	t.Run("process termination leaves recoverable dirty state", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "process_crash")
		childURL := databaseURL(t, connectionString, "test_process_crash")
		command := migrationHelperCommand(childURL, "crash")
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if err := command.Start(); err != nil {
			t.Fatalf("start crash process: %v", err)
		}

		deadline := time.Now().Add(10 * time.Second)
		for {
			var ready bool
			err := database.QueryRowContext(
				context.Background(),
				`SELECT EXISTS (SELECT 1 FROM public.go_schema_migrations WHERE dirty)`,
			).Scan(&ready)
			if err == nil && ready {
				break
			}
			if time.Now().After(deadline) {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("crash boundary was not reached: %v\n%s", err, output.String())
			}
			time.Sleep(25 * time.Millisecond)
		}
		if err := command.Process.Kill(); err != nil {
			t.Fatalf("kill migration process: %v", err)
		}
		if err := command.Wait(); err == nil {
			t.Fatal("killed migration process exited successfully")
		}
		waitForAdvisoryUnlock(t, database)

		files := crashMigrationFiles()
		runner := newIntegrationRunner(t, database, files)
		var status migrations.Status
		deadline = time.Now().Add(15 * time.Second)
		for {
			status, err = runner.Status(context.Background())
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Status() after process termination error = %v", err)
			}
			time.Sleep(25 * time.Millisecond)
		}
		if len(status.Entries()) != 1 || status.Entries()[0].State() != migrations.StateDirty {
			t.Fatalf("Status() = %#v, want dirty", status.Entries())
		}
		if _, err := database.ExecContext(context.Background(), "DROP TABLE IF EXISTS process_partial"); err != nil {
			t.Fatalf("remove partial process effect: %v", err)
		}
		source, err := migrations.NewFSSource(files, "migrations")
		if err != nil {
			t.Fatalf("NewFSSource() error = %v", err)
		}
		loaded, err := source.Load(context.Background())
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		recovery, err := migrations.NewRecovery(
			loaded[0].Version(),
			loaded[0].Checksum(),
			migrations.RecoveryMarkRolledBack,
		)
		if err != nil {
			t.Fatalf("NewRecovery() error = %v", err)
		}
		if _, err := runner.Recover(context.Background(), recovery); err != nil {
			t.Fatalf("Recover() error = %v", err)
		}
	})

	t.Run("process termination rolls back transactional statement and ledger", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "transaction_crash")
		childURL := databaseURL(t, connectionString, "test_transaction_crash")
		command := migrationHelperCommand(childURL, "transaction-crash")
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if err := command.Start(); err != nil {
			t.Fatalf("start transactional crash process: %v", err)
		}
		waitForDatabaseCondition(t, database, command, &output, `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
      AND state = 'active'
      AND query LIKE '%process_transactional%'
)`)
		if err := command.Process.Kill(); err != nil {
			t.Fatalf("kill transactional migration process: %v", err)
		}
		if err := command.Wait(); err == nil {
			t.Fatal("killed transactional migration process exited successfully")
		}
		waitForAdvisoryUnlock(t, database)

		var tableExists bool
		var ledgerRows int
		if err := database.QueryRowContext(
			context.Background(),
			"SELECT to_regclass('public.process_transactional') IS NOT NULL",
		).Scan(&tableExists); err != nil {
			t.Fatalf("inspect transactional crash table: %v", err)
		}
		if err := database.QueryRowContext(
			context.Background(),
			"SELECT count(*) FROM public.go_schema_migrations",
		).Scan(&ledgerRows); err != nil {
			t.Fatalf("inspect transactional crash ledger: %v", err)
		}
		if tableExists || ledgerRows != 0 {
			t.Fatalf("transactional crash table=%t ledger rows=%d", tableExists, ledgerRows)
		}

		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000001_process_transactional.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nCREATE TABLE process_transactional (id bigint);\n",
			)},
		})
		if _, err := runner.Up(context.Background()); err != nil {
			t.Fatalf("retry after transactional crash: %v", err)
		}
	})

	t.Run("process termination during clean ledger write remains recoverable", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "clean_update_crash")
		backend, err := migrationpostgres.New(database)
		if err != nil {
			t.Fatalf("postgres.New() error = %v", err)
		}
		session, err := backend.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire() error = %v", err)
		}
		if err := session.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if err := session.Release(context.Background()); err != nil {
			t.Fatalf("Release() error = %v", err)
		}
		if _, err := database.ExecContext(context.Background(), `
CREATE FUNCTION pause_clean_ledger_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.dirty AND NOT NEW.dirty THEN
        PERFORM pg_sleep(30);
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER pause_clean_ledger_update
BEFORE UPDATE ON public.go_schema_migrations
FOR EACH ROW EXECUTE FUNCTION pause_clean_ledger_update();
`); err != nil {
			t.Fatalf("install clean-update crash trigger: %v", err)
		}

		childURL := databaseURL(t, connectionString, "test_clean_update_crash")
		command := migrationHelperCommand(childURL, "clean-update-crash")
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if err := command.Start(); err != nil {
			t.Fatalf("start clean-update crash process: %v", err)
		}
		waitForDatabaseCondition(t, database, command, &output, `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
      AND state = 'active'
      AND query LIKE 'UPDATE public.go_schema_migrations SET finished_at%'
)`)
		if err := command.Process.Kill(); err != nil {
			t.Fatalf("kill clean-update migration process: %v", err)
		}
		if err := command.Wait(); err == nil {
			t.Fatal("killed clean-update migration process exited successfully")
		}
		waitForAdvisoryUnlock(t, database)
		if _, err := database.ExecContext(context.Background(), `
DROP TRIGGER pause_clean_ledger_update ON public.go_schema_migrations;
DROP FUNCTION pause_clean_ledger_update();
`); err != nil {
			t.Fatalf("remove clean-update crash trigger: %v", err)
		}
		var dirty bool
		if err := database.QueryRowContext(
			context.Background(),
			"SELECT dirty FROM public.go_schema_migrations WHERE version = 1",
		).Scan(&dirty); err != nil {
			t.Fatalf("query interrupted clean ledger row: %v", err)
		}
		if !dirty {
			t.Fatal("interrupted clean ledger update was committed")
		}

		files := cleanUpdateMigrationFiles()
		source, err := migrations.NewFSSource(files, "migrations")
		if err != nil {
			t.Fatalf("NewFSSource() error = %v", err)
		}
		loaded, err := source.Load(context.Background())
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		recovery, err := migrations.NewRecovery(
			loaded[0].Version(),
			loaded[0].Checksum(),
			migrations.RecoveryMarkApplied,
		)
		if err != nil {
			t.Fatalf("NewRecovery() error = %v", err)
		}
		runner := newIntegrationRunner(t, database, files)
		if _, err := runner.Recover(context.Background(), recovery); err != nil {
			t.Fatalf("Recover(mark applied) error = %v", err)
		}
	})

	t.Run("transaction failure rolls back", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "transaction_failure")
		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000001_fail.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nCREATE TABLE must_rollback (id bigint);\nSELECT missing_column;\n",
			)},
		})
		if _, err := runner.Up(context.Background()); err == nil {
			t.Fatal("Up() error = nil, want transactional failure")
		}
		var exists bool
		if err := database.QueryRowContext(
			context.Background(),
			"SELECT to_regclass('public.must_rollback') IS NOT NULL",
		).Scan(&exists); err != nil {
			t.Fatalf("inspect rolled-back table: %v", err)
		}
		if exists {
			t.Fatal("transactional DDL survived failed migration")
		}
	})

	t.Run("no transaction failure is dirty and recoverable", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "dirty")
		files := fstest.MapFS{
			"migrations/000001_partial.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations NoTransaction\n-- +migrations Up\n" +
					"CREATE TABLE partial_effect (id bigint);\nSELECT missing_column;\n",
			)},
		}
		runner := newIntegrationRunner(t, database, files)
		if _, err := runner.Up(context.Background()); err == nil {
			t.Fatal("Up() error = nil, want partial failure")
		}
		status, err := runner.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if len(status.Entries()) != 1 || status.Entries()[0].State() != migrations.StateDirty {
			t.Fatalf("Status() = %#v, want one dirty entry", status.Entries())
		}
		if _, err := database.ExecContext(context.Background(), "DROP TABLE IF EXISTS partial_effect"); err != nil {
			t.Fatalf("remove reviewed partial effect: %v", err)
		}
		source, err := migrations.NewFSSource(files, "migrations")
		if err != nil {
			t.Fatalf("NewFSSource() error = %v", err)
		}
		loaded, err := source.Load(context.Background())
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		recovery, err := migrations.NewRecovery(
			loaded[0].Version(),
			loaded[0].Checksum(),
			migrations.RecoveryMarkRolledBack,
		)
		if err != nil {
			t.Fatalf("NewRecovery() error = %v", err)
		}
		if _, err := runner.Recover(context.Background(), recovery); err != nil {
			t.Fatalf("Recover() error = %v", err)
		}
	})

	t.Run("baseline rejects drift then preserves Laravel history", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "baseline")
		if _, err := database.ExecContext(
			context.Background(),
			"CREATE TABLE migrations (id serial PRIMARY KEY, migration text NOT NULL, batch integer NOT NULL);"+
				"INSERT INTO migrations (migration, batch) VALUES ('2020_01_01_000000_create_users', 1);"+
				"CREATE TABLE users (id bigint PRIMARY KEY);",
		); err != nil {
			t.Fatalf("create Laravel schema: %v", err)
		}
		backend, err := migrationpostgres.New(database)
		if err != nil {
			t.Fatalf("postgres.New() error = %v", err)
		}
		fingerprint, err := backend.Inspect(context.Background())
		if err != nil {
			t.Fatalf("Inspect() error = %v", err)
		}
		beforeObjects, err := backend.InspectObjects(context.Background())
		if err != nil {
			t.Fatalf("InspectObjects() error = %v", err)
		}
		if _, err := database.ExecContext(context.Background(), "ALTER TABLE users ADD COLUMN drift text"); err != nil {
			t.Fatalf("introduce drift: %v", err)
		}
		baseline, err := migrations.NewBaseline(100, "laravel_production_v1", fingerprint)
		if err != nil {
			t.Fatalf("NewBaseline() error = %v", err)
		}
		runner := newIntegrationRunner(t, database, fstest.MapFS{
			"migrations/000101_go_table.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nCREATE TABLE go_owned (id bigint);\n")},
		})
		if _, err := runner.Baseline(context.Background(), baseline); !errors.Is(err, migrations.ErrBaselineMismatch) {
			t.Fatalf("Baseline(drift) error = %v, want ErrBaselineMismatch", err)
		}
		if _, err := database.ExecContext(context.Background(), "ALTER TABLE users DROP COLUMN drift"); err != nil {
			t.Fatalf("remove drift: %v", err)
		}
		afterObjects, err := backend.InspectObjects(context.Background())
		if err != nil {
			t.Fatalf("InspectObjects(restored) error = %v", err)
		}
		if differences := schemaDifferences(beforeObjects, afterObjects); len(differences) != 0 {
			t.Fatalf("restored schema differences: %v", differences)
		}
		if _, err := runner.Baseline(context.Background(), baseline); err != nil {
			t.Fatalf("Baseline() error = %v", err)
		}
		var laravelRows int
		if err := database.QueryRowContext(context.Background(), "SELECT count(*) FROM migrations").Scan(&laravelRows); err != nil {
			t.Fatalf("query Laravel migrations: %v", err)
		}
		if laravelRows != 1 {
			t.Fatalf("Laravel migration rows = %d, want 1", laravelRows)
		}
	})

	t.Run("baseline fixtures fail closed outside exact Laravel schema", func(t *testing.T) {
		expectedDatabase := isolatedDatabase(t, admin, connectionString, "baseline_expected")
		if _, err := expectedDatabase.ExecContext(
			context.Background(),
			readSQLFixture(t, "testdata/laravel/exact.sql"),
		); err != nil {
			t.Fatalf("install exact Laravel fixture: %v", err)
		}
		expectedBackend, err := migrationpostgres.New(expectedDatabase)
		if err != nil {
			t.Fatalf("postgres.New(expected) error = %v", err)
		}
		expectedFingerprint, err := expectedBackend.Inspect(context.Background())
		if err != nil {
			t.Fatalf("Inspect(expected) error = %v", err)
		}
		baseline, err := migrations.NewBaseline(
			100,
			"laravel_production_v1",
			expectedFingerprint,
		)
		if err != nil {
			t.Fatalf("NewBaseline() error = %v", err)
		}

		for _, test := range []struct {
			name      string
			fixture   string
			wantError bool
		}{
			{name: "empty", fixture: "empty.sql", wantError: true},
			{name: "exact", fixture: "exact.sql"},
			{name: "drifted", fixture: "drifted.sql", wantError: true},
			{name: "partial", fixture: "partial.sql", wantError: true},
			{name: "unexpectedly advanced", fixture: "advanced.sql", wantError: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				database := isolatedDatabase(
					t,
					admin,
					connectionString,
					"baseline_"+strings.ReplaceAll(test.name, " ", "_"),
				)
				if _, err := database.ExecContext(
					context.Background(),
					readSQLFixture(t, "testdata/laravel/"+test.fixture),
				); err != nil {
					t.Fatalf("install Laravel fixture: %v", err)
				}
				before := laravelHistory(t, database)
				runner := newIntegrationRunner(t, database, fstest.MapFS{
					"migrations/000101_after_baseline.sql": &fstest.MapFile{Data: []byte(
						"-- +migrations Up\nSELECT 1;\n",
					)},
				})
				_, baselineErr := runner.Baseline(context.Background(), baseline)
				if test.wantError && !errors.Is(baselineErr, migrations.ErrBaselineMismatch) {
					t.Fatalf("Baseline() error = %v, want ErrBaselineMismatch", baselineErr)
				}
				if !test.wantError && baselineErr != nil {
					t.Fatalf("Baseline() error = %v", baselineErr)
				}
				if after := laravelHistory(t, database); after != before {
					t.Fatalf("Laravel history changed from %q to %q", before, after)
				}
			})
		}
	})

	t.Run("statement timeout leaves transactional history retryable", func(t *testing.T) {
		database := isolatedDatabase(t, admin, connectionString, "timeout")
		source, err := migrations.NewFSSource(fstest.MapFS{
			"migrations/000001_sleep.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT pg_sleep(1);\n")},
		}, "migrations")
		if err != nil {
			t.Fatalf("NewFSSource() error = %v", err)
		}
		backend, err := migrationpostgres.New(
			database,
			migrationpostgres.WithStatementTimeout(50*time.Millisecond),
		)
		if err != nil {
			t.Fatalf("postgres.New() error = %v", err)
		}
		runner, err := migrations.NewRunner(source, backend)
		if err != nil {
			t.Fatalf("NewRunner() error = %v", err)
		}
		if _, err := runner.Up(context.Background()); err == nil {
			t.Fatal("Up() error = nil, want statement timeout")
		}
		var count int
		if err := database.QueryRowContext(context.Background(), "SELECT count(*) FROM public.go_schema_migrations").Scan(&count); err != nil {
			t.Fatalf("count timeout ledger: %v", err)
		}
		if count != 0 {
			t.Fatalf("timeout ledger count = %d, want 0", count)
		}
	})
}

func TestPostgresSubprocessMigrationHelper(t *testing.T) {
	if os.Getenv("GO_MIGRATIONS_SUBPROCESS") != "1" {
		t.Skip("subprocess helper")
	}
	database := openDatabase(t, os.Getenv("DATABASE_URL"))
	var files fstest.MapFS
	switch os.Getenv("GO_MIGRATIONS_SUBPROCESS_MODE") {
	case "concurrent":
		files = fstest.MapFS{
			"migrations/000001_process.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\nSELECT pg_sleep(0.2);\nCREATE TABLE process_once (id bigint);\n",
			)},
		}
	case "crash":
		files = crashMigrationFiles()
	case "transaction-crash":
		files = fstest.MapFS{
			"migrations/000001_process_transactional.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\n" +
					"CREATE TABLE process_transactional (id bigint);\n" +
					"SELECT pg_sleep(30);\n",
			)},
		}
	case "clean-update-crash":
		files = cleanUpdateMigrationFiles()
	default:
		t.Fatal("unknown subprocess mode")
	}
	runner := newIntegrationRunner(t, database, files)
	if _, err := runner.Up(context.Background()); err != nil {
		t.Fatalf("subprocess Up() error = %v", err)
	}
}

func migrationHelperCommand(connectionString string, mode string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestPostgresSubprocessMigrationHelper$")
	command.Env = append(
		os.Environ(),
		"GO_MIGRATIONS_SUBPROCESS=1",
		"GO_MIGRATIONS_SUBPROCESS_MODE="+mode,
		"DATABASE_URL="+connectionString,
	)

	return command
}

func crashMigrationFiles() fstest.MapFS {
	return fstest.MapFS{
		"migrations/000001_process_partial.sql": &fstest.MapFile{Data: []byte(
			"-- +migrations NoTransaction\n-- +migrations Up\n" +
				"CREATE TABLE process_partial (id bigint);\nSELECT pg_sleep(30);\n",
		)},
	}
}

func cleanUpdateMigrationFiles() fstest.MapFS {
	return fstest.MapFS{
		"migrations/000001_process_clean_update.sql": &fstest.MapFile{Data: []byte(
			"-- +migrations NoTransaction\n-- +migrations Up\n" +
				"CREATE TABLE process_clean_update (id bigint);\n",
		)},
	}
}

func newIntegrationRunner(
	t *testing.T,
	database *sql.DB,
	files fstest.MapFS,
) *migrations.Runner {
	t.Helper()

	source, err := migrations.NewFSSource(files, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	backend, err := migrationpostgres.New(
		database,
		migrationpostgres.WithLockTimeout(5*time.Second),
		migrationpostgres.WithStatementTimeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("postgres.New() error = %v", err)
	}
	runner, err := migrations.NewRunner(source, backend)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	return runner
}

func isolatedDatabase(
	t *testing.T,
	admin *sql.DB,
	connectionString string,
	suffix string,
) *sql.DB {
	t.Helper()

	name := "test_" + strings.ReplaceAll(suffix, "-", "_")
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	return openDatabase(t, databaseURL(t, connectionString, name))
}

func databaseURL(t *testing.T, connectionString string, databaseName string) string {
	t.Helper()

	parsed, err := url.Parse(connectionString)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}
	parsed.Path = "/" + databaseName

	return parsed.String()
}

func openDatabase(t *testing.T, connectionString string) *sql.DB {
	t.Helper()

	database, err := sql.Open("pgx", connectionString)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database ping: %v", err)
	}

	return database
}

func waitForDatabaseCondition(
	t *testing.T,
	database *sql.DB,
	command *exec.Cmd,
	output *bytes.Buffer,
	query string,
) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var ready bool
		err := database.QueryRowContext(context.Background(), query).Scan(&ready)
		if err == nil && ready {
			return
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("process boundary was not reached: %v\n%s", err, output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitForAdvisoryUnlock(t *testing.T, database *sql.DB) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var released bool
		err := database.QueryRowContext(context.Background(), `
SELECT NOT EXISTS (
    SELECT 1
    FROM pg_locks locks
    JOIN pg_stat_activity activity ON activity.pid = locks.pid
    WHERE locks.locktype = 'advisory'
      AND locks.granted
      AND activity.datname = current_database()
)
`).Scan(&released)
		if err == nil && released {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("advisory lock remained after process exit: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readSQLFixture(t *testing.T, filename string) string {
	t.Helper()

	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read SQL fixture %s: %v", filename, err)
	}

	return string(contents)
}

func laravelHistory(t *testing.T, database *sql.DB) string {
	t.Helper()

	var history string
	if err := database.QueryRowContext(
		context.Background(),
		"SELECT COALESCE(string_agg(migration || ':' || batch::text, ',' ORDER BY id), '') FROM migrations",
	).Scan(&history); err != nil {
		t.Fatalf("query Laravel history: %v", err)
	}

	return history
}

func schemaDifferences(
	before []migrationpostgres.SchemaObject,
	after []migrationpostgres.SchemaObject,
) []string {
	beforeMap := make(map[string]string, len(before))
	for _, object := range before {
		beforeMap[object.Identity] = object.Definition
	}
	afterMap := make(map[string]string, len(after))
	for _, object := range after {
		afterMap[object.Identity] = object.Definition
	}
	differences := make([]string, 0)
	for identity, definition := range beforeMap {
		if afterMap[identity] != definition {
			differences = append(differences, "changed-or-missing:"+identity)
		}
	}
	for identity := range afterMap {
		if _, exists := beforeMap[identity]; !exists {
			differences = append(differences, "added:"+identity)
		}
	}

	return differences
}
