package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	migrations "github.com/faustbrian/golib/pkg/migrations"
	"github.com/faustbrian/golib/pkg/migrations/postgres"
)

func TestSessionPrepareCreatesOnlyOwnedLedgerOnLockConnection(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	backend, err := postgres.New(database)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	session, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS public.go_schema_migrations")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := session.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestBackendSessionSerializesWorkWithAdvisoryLock(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	backend, err := postgres.New(
		database,
		postgres.WithLockRetryInterval(time.Microsecond),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))

	session, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := session.Release(context.Background()); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestBackendLockTimeoutCancelsPollingWithoutAcquiring(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	backend, err := postgres.New(
		database,
		postgres.WithLockRetryInterval(time.Second),
		postgres.WithLockTimeout(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

	_, err = backend.Acquire(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want context deadline", err)
	}
	assertExpectations(t, mock)
}

func TestSessionAppliesTransactionalMigrationAtomically(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	session := acquireSession(t, database, mock)
	migration := newMigration(t, migrations.TransactionModeDefault)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO public.go_schema_migrations.*'postgres'.*'v1'").
		WithArgs(
			int64(1),
			"migration",
			"create_users",
			migration.Checksum().String(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE public.go_schema_migrations").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1), migration.Checksum().String()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	record, err := session.Apply(context.Background(), migration)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if record.Version() != 1 || record.Dirty() {
		t.Fatalf("Apply() record = %#v", record)
	}
	assertExpectations(t, mock)
}

func TestSessionSetsTransactionLocalStatementTimeout(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	backend, err := postgres.New(database, postgres.WithStatementTimeout(250*time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	session, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	migration := newMigration(t, migrations.TransactionModeDefault)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT set_config('statement_timeout', $1, true)")).
		WithArgs("250ms").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := session.Apply(context.Background(), migration); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestSessionLeavesNoTransactionMigrationDirtyAfterFailure(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	session := acquireSession(t, database, mock)
	migration := newMigration(t, migrations.TransactionModeNone)
	executionError := errors.New("index build lost connection")

	mock.ExpectExec("INSERT INTO public.go_schema_migrations").
		WithArgs(
			int64(1),
			"migration",
			"create_users",
			migration.Checksum().String(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnError(executionError)

	_, err := session.Apply(context.Background(), migration)
	if !errors.Is(err, executionError) {
		t.Fatalf("Apply() error = %v, want execution error", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "goose") {
		t.Fatalf("Apply() leaked adapter identity: %v", err)
	}
	assertExpectations(t, mock)
}

func TestNoTransactionStatementTimeoutIsResetAfterFailure(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	backend, err := postgres.New(database, postgres.WithStatementTimeout(250*time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	session, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	migration := newMigration(t, migrations.TransactionModeNone)
	executionError := errors.New("statement timeout")
	mock.ExpectExec(regexp.QuoteMeta("SELECT set_config('statement_timeout', $1, false)")).
		WithArgs("250ms").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnError(executionError)
	mock.ExpectExec(regexp.QuoteMeta("SELECT set_config('statement_timeout', '0', false)")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	_, err = session.Apply(context.Background(), migration)
	if !errors.Is(err, executionError) {
		t.Fatalf("Apply() error = %v, want execution error", err)
	}
	assertExpectations(t, mock)
}

func TestSessionRollsBackTransactionalMigrationAtomically(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	session := acquireSession(t, database, mock)
	migration := newMigration(t, migrations.TransactionModeDefault)
	finishedAt := time.Unix(1_700_000_000, 0).UTC()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("DELETE FROM public.go_schema_migrations").
		WithArgs(int64(1), migration.Checksum().String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"finished_at",
			"execution_time_ms",
		}).AddRow(finishedAt, int64(12)))
	mock.ExpectCommit()

	record, err := session.Rollback(context.Background(), migration)
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if record.Version() != 1 || record.AppliedAt() != finishedAt {
		t.Fatalf("Rollback() record = %#v", record)
	}
	assertExpectations(t, mock)
}

func TestSessionRejectsMalformedLedgerRows(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	session := acquireSession(t, database, mock)
	mock.ExpectQuery("SELECT (.+) FROM public.go_schema_migrations").WillReturnRows(
		sqlmock.NewRows([]string{
			"kind",
			"version",
			"name",
			"checksum",
			"started_at",
			"finished_at",
			"execution_time_ms",
			"dirty",
		}).AddRow(
			"migration",
			int64(1),
			"create_users",
			"not-a-checksum",
			time.Now().UTC(),
			time.Now().UTC(),
			int64(1),
			false,
		),
	)

	_, err := session.Records(context.Background())
	if !errors.Is(err, migrations.ErrInvalidRecord) {
		t.Fatalf("Records() error = %v, want ErrInvalidRecord", err)
	}
	assertExpectations(t, mock)
}

func TestSessionRejectsInconsistentLedgerCompletionState(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		finishedAt any
		dirty      bool
	}{
		{name: "finished dirty row", finishedAt: time.Now().UTC(), dirty: true},
		{name: "unfinished clean row", finishedAt: nil, dirty: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, mock := newMockDatabase(t)
			session := acquireSession(t, database, mock)
			migration := newMigration(t, migrations.TransactionModeDefault)
			mock.ExpectQuery("SELECT (.+) FROM public.go_schema_migrations").WillReturnRows(
				sqlmock.NewRows([]string{
					"kind",
					"version",
					"name",
					"checksum",
					"started_at",
					"finished_at",
					"execution_time_ms",
					"dirty",
				}).AddRow(
					"migration",
					int64(1),
					"create_users",
					migration.Checksum().String(),
					time.Now().UTC(),
					test.finishedAt,
					int64(1),
					test.dirty,
				),
			)

			_, err := session.Records(context.Background())
			if !errors.Is(err, migrations.ErrInvalidRecord) {
				t.Fatalf("Records() error = %v, want ErrInvalidRecord", err)
			}
			assertExpectations(t, mock)
		})
	}
}

func TestFingerprintCanonicalizesReviewedSchemaObjects(t *testing.T) {
	t.Parallel()

	fingerprint, err := postgres.Fingerprint([]postgres.SchemaObject{
		{Identity: "table:public.users", Definition: "relkind=r persistence=p"},
		{Identity: "column:public.users.id", Definition: "bigint not null"},
	})
	if err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	const expected = "sha256:bae3fe316f2f78f8e2d9a2095f08e67ac13e74965081575f1aca1c1d5bb3c04e"
	if fingerprint.String() != expected {
		t.Fatalf("Fingerprint() = %q, want %q", fingerprint, expected)
	}
}

func TestSessionRecordsBaselineOnlyAfterExactSchemaMatch(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	session := acquireSession(t, database, mock)
	objects := []postgres.SchemaObject{
		{Identity: "column:public.users.id", Definition: "bigint not null"},
		{Identity: "table:public.users", Definition: "relkind=r persistence=p"},
	}
	fingerprint, err := postgres.Fingerprint(objects)
	if err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	baseline, err := migrations.NewBaseline(100, "laravel_production_v1", fingerprint)
	if err != nil {
		t.Fatalf("NewBaseline() error = %v", err)
	}
	capable, ok := session.(interface {
		Baseline(context.Context, migrations.Baseline) (migrations.Record, error)
	})
	if !ok {
		t.Fatal("PostgreSQL session does not implement baseline contract")
	}

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").
		WillReturnRows(sqlmock.NewRows([]string{"object_identity", "definition"}).
			AddRow(objects[0].Identity, objects[0].Definition).
			AddRow(objects[1].Identity, objects[1].Definition))
	mock.ExpectExec("INSERT INTO public.go_schema_migrations").
		WithArgs(
			int64(100),
			"baseline",
			"laravel_production_v1",
			fingerprint.String(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	record, err := capable.Baseline(context.Background(), baseline)
	if err != nil {
		t.Fatalf("Baseline() error = %v", err)
	}
	if record.Kind() != migrations.RecordKindBaseline || record.Checksum() != fingerprint {
		t.Fatalf("Baseline() record = %#v", record)
	}
	assertExpectations(t, mock)
}

func TestSessionResolvesDirtyMigrationWithoutManualLedgerEdit(t *testing.T) {
	t.Parallel()

	database, mock := newMockDatabase(t)
	session := acquireSession(t, database, mock)
	migration := newMigration(t, migrations.TransactionModeNone)
	capable, ok := session.(interface {
		Recover(context.Context, migrations.Migration, migrations.RecoveryAction) (migrations.Record, error)
	})
	if !ok {
		t.Fatal("PostgreSQL session does not implement recovery contract")
	}
	startedAt := time.Now().UTC().Add(-time.Second)
	mock.ExpectQuery("UPDATE public.go_schema_migrations").
		WithArgs(sqlmock.AnyArg(), int64(1), migration.Checksum().String()).
		WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(startedAt))

	record, err := capable.Recover(
		context.Background(),
		migration,
		migrations.RecoveryMarkApplied,
	)
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if record.Dirty() || record.Version() != migration.Version() {
		t.Fatalf("Recover() record = %#v", record)
	}
	if record.Duration()%time.Millisecond != 0 {
		t.Fatalf("Recover() duration = %v, want persisted millisecond precision", record.Duration())
	}
	assertExpectations(t, mock)
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := postgres.New(nil); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatalf("New(nil) error = %v, want ErrInvalidConfig", err)
	}
	if _, err := postgres.New(&sql.DB{}, postgres.WithLockTimeout(0)); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatalf("New(lock timeout) error = %v, want ErrInvalidConfig", err)
	}
	if _, err := postgres.New(&sql.DB{}, postgres.WithStatementTimeout(0)); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatalf("New(statement timeout) error = %v, want ErrInvalidConfig", err)
	}
	if _, err := postgres.New(&sql.DB{}, postgres.WithStatementTimeout(time.Nanosecond)); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatalf("New(sub-millisecond statement timeout) error = %v, want ErrInvalidConfig", err)
	}
}

func acquireSession(
	t *testing.T,
	database *sql.DB,
	mock sqlmock.Sqlmock,
) migrations.Session {
	t.Helper()

	backend, err := postgres.New(database)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))

	session, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	return session
}

func newMigration(t *testing.T, mode migrations.TransactionMode) migrations.Migration {
	t.Helper()

	migration, err := migrations.NewMigration(
		1,
		"create_users",
		mode,
		"CREATE TABLE users (id bigint PRIMARY KEY);",
		"DROP TABLE users;",
	)
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}

	return migration
}

func newMockDatabase(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()

	database, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	return database, mock
}

func assertExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}
