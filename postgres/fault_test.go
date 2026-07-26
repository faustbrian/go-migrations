package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func TestTransactionalApplyStopsAtEveryPersistenceBoundary(t *testing.T) {
	t.Parallel()

	fault := errors.New("injected persistence fault")
	tests := []struct {
		name  string
		setup func(sqlmock.Sqlmock, migrations.Migration)
	}{
		{
			name: "begin",
			setup: func(mock sqlmock.Sqlmock, _ migrations.Migration) {
				mock.ExpectBegin().WillReturnError(fault)
			},
		},
		{
			name: "statement timeout",
			setup: func(mock sqlmock.Sqlmock, _ migrations.Migration) {
				mock.ExpectBegin()
				mock.ExpectExec("set_config").WillReturnError(fault)
				mock.ExpectRollback()
			},
		},
		{
			name: "dirty ledger insert",
			setup: func(mock sqlmock.Sqlmock, _ migrations.Migration) {
				mock.ExpectBegin()
				mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnError(fault)
				mock.ExpectRollback()
			},
		},
		{
			name: "migration statement",
			setup: func(mock sqlmock.Sqlmock, migration migrations.Migration) {
				mock.ExpectBegin()
				mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnError(fault)
				mock.ExpectRollback()
			},
		},
		{
			name: "clean ledger update",
			setup: func(mock sqlmock.Sqlmock, migration migrations.Migration) {
				mock.ExpectBegin()
				mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec("UPDATE public.go_schema_migrations").WillReturnError(fault)
				mock.ExpectRollback()
			},
		},
		{
			name: "commit",
			setup: func(mock sqlmock.Sqlmock, migration migrations.Migration) {
				mock.ExpectBegin()
				mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec("UPDATE public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit().WillReturnError(fault)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			session, mock := faultSession(t, 100*time.Millisecond)
			migration := faultMigration(t, migrations.TransactionModeDefault)
			test.setup(mock, migration)

			if _, err := session.Apply(context.Background(), migration); err == nil {
				t.Fatal("Apply() error = nil, want injected failure")
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestLedgerMutationHelpersFailClosed(t *testing.T) {
	t.Parallel()

	fault := errors.New("injected persistence fault")
	migration := faultMigration(t, migrations.TransactionModeDefault)
	now := time.Now().UTC()

	t.Run("insert", func(t *testing.T) {
		database, mock := faultDatabase(t)
		mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnError(fault)
		if err := insertDirty(context.Background(), database, migration, now); !errors.Is(err, fault) {
			t.Fatalf("insertDirty() error = %v", err)
		}
		assertFaultExpectations(t, mock)
	})

	for _, test := range []struct {
		name   string
		result sql.Result
		err    error
	}{
		{name: "update", err: fault},
		{name: "result", result: sqlmock.NewErrorResult(fault)},
		{name: "conflict", result: sqlmock.NewResult(0, 0)},
	} {
		t.Run("complete "+test.name, func(t *testing.T) {
			database, mock := faultDatabase(t)
			expectation := mock.ExpectExec("UPDATE public.go_schema_migrations")
			if test.err != nil {
				expectation.WillReturnError(test.err)
			} else {
				expectation.WillReturnResult(test.result)
			}
			if err := markClean(context.Background(), database, migration, now, time.Millisecond); err == nil {
				t.Fatal("markClean() error = nil, want failure")
			}
			assertFaultExpectations(t, mock)
		})
	}

	for _, test := range []struct {
		name string
		rows *sqlmock.Rows
		err  error
	}{
		{name: "missing", rows: sqlmock.NewRows([]string{"finished_at", "execution_time_ms"})},
		{name: "query", err: fault},
		{name: "malformed", rows: sqlmock.NewRows([]string{"finished_at", "execution_time_ms"}).AddRow("bad-time", 1)},
	} {
		t.Run("delete "+test.name, func(t *testing.T) {
			database, mock := faultDatabase(t)
			expectation := mock.ExpectQuery("DELETE FROM public.go_schema_migrations")
			if test.err != nil {
				expectation.WillReturnError(test.err)
			} else {
				expectation.WillReturnRows(test.rows)
			}
			if _, err := deleteRecord(context.Background(), database, migration); err == nil {
				t.Fatal("deleteRecord() error = nil, want failure")
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestNoTransactionRollbackLeavesRecoverableDirtyRecordOnFailure(t *testing.T) {
	t.Parallel()

	fault := errors.New("injected rollback fault")
	session, mock := faultSession(t, 0)
	migration := faultMigration(t, migrations.TransactionModeNone)
	appliedAt := time.Now().UTC()
	mock.ExpectQuery("UPDATE public.go_schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"started_at", "execution_time_ms"}).AddRow(appliedAt, 12))
	mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnError(fault)

	if _, err := session.Rollback(context.Background(), migration); !errors.Is(err, fault) {
		t.Fatalf("Rollback() error = %v, want injected failure", err)
	}
	assertFaultExpectations(t, mock)
}

func TestBackendAndLockFailuresAreExplicit(t *testing.T) {
	t.Parallel()

	fault := errors.New("injected backend fault")
	var nilBackend *Backend
	if _, err := nilBackend.Acquire(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Acquire() error = %v", err)
	}

	database, mock := faultDatabase(t)
	backend, err := New(database)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	prepareSession, prepareMock := faultSession(t, 0)
	prepareMock.ExpectExec("CREATE TABLE IF NOT EXISTS public.go_schema_migrations").WillReturnError(fault)
	if err := prepareSession.Prepare(context.Background()); !errors.Is(err, fault) {
		t.Fatalf("Prepare() error = %v, want injected fault", err)
	}
	assertFaultExpectations(t, prepareMock)
	mock.ExpectQuery("pg_try_advisory_lock").WillReturnError(fault)
	if _, err := backend.Acquire(context.Background()); !errors.Is(err, fault) {
		t.Fatalf("Acquire() error = %v, want injected fault", err)
	}
	assertFaultExpectations(t, mock)

	closed, closedMock := faultDatabase(t)
	closedMock.ExpectClose()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	closedBackend, err := New(closed)
	if err != nil {
		t.Fatalf("New(closed) error = %v", err)
	}
	if _, err := closedBackend.Acquire(context.Background()); err == nil {
		t.Fatal("Acquire(closed) error = nil")
	}
}

func TestLockQueryCancellationReturnsContextError(t *testing.T) {
	t.Parallel()

	database, mock := faultDatabase(t)
	backend, err := New(database, WithLockTimeout(time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mock.ExpectQuery("pg_try_advisory_lock").
		WillDelayFor(50 * time.Millisecond).
		WillReturnRows(sqlmock.NewRows([]string{"acquired"}).AddRow(true))
	if _, err := backend.Acquire(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want context deadline", err)
	}
	assertFaultExpectations(t, mock)
}

func TestSessionReleaseDetectsLostOrRepeatedOwnership(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		rows     *sqlmock.Rows
		queryErr error
		target   error
	}{
		{name: "query", queryErr: errors.New("unlock failed")},
		{name: "not held", rows: sqlmock.NewRows([]string{"unlocked"}).AddRow(false), target: ErrLockNotHeld},
		{name: "success", rows: sqlmock.NewRows([]string{"unlocked"}).AddRow(true), target: ErrSessionReleased},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, mock := faultSession(t, 0)
			expectation := mock.ExpectQuery("pg_advisory_unlock")
			if test.queryErr != nil {
				expectation.WillReturnError(test.queryErr)
			} else {
				expectation.WillReturnRows(test.rows)
			}
			err := session.Release(context.Background())
			if test.name == "query" && !errors.Is(err, test.queryErr) {
				t.Fatalf("Release() error = %v", err)
			}
			if test.name == "not held" && !errors.Is(err, test.target) {
				t.Fatalf("Release() error = %v, want %v", err, test.target)
			}
			if test.name == "success" {
				if err != nil {
					t.Fatalf("Release() error = %v", err)
				}
				if err := session.Release(context.Background()); !errors.Is(err, test.target) {
					t.Fatalf("second Release() error = %v, want %v", err, test.target)
				}
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestSchemaInspectionFailuresAndSnapshots(t *testing.T) {
	t.Parallel()

	var nilBackend *Backend
	if _, err := nilBackend.Inspect(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Inspect() error = %v", err)
	}
	if _, err := nilBackend.InspectObjects(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil InspectObjects() error = %v", err)
	}

	t.Run("objects", func(t *testing.T) {
		database, mock := faultDatabase(t)
		backend, err := New(database)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").
			WillReturnRows(sqlmock.NewRows([]string{"object_identity", "definition"}).
				AddRow("table:public.users", "table definition"))
		objects, err := backend.InspectObjects(context.Background())
		if err != nil || len(objects) != 1 || objects[0].Identity != "table:public.users" {
			t.Fatalf("InspectObjects() = %#v, %v", objects, err)
		}
		assertFaultExpectations(t, mock)
	})

	t.Run("fingerprint", func(t *testing.T) {
		database, mock := faultDatabase(t)
		backend, err := New(database)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").
			WillReturnRows(sqlmock.NewRows([]string{"object_identity", "definition"}).
				AddRow("table:public.users", "table definition"))
		fingerprint, err := backend.Inspect(context.Background())
		if err != nil || fingerprint == (migrations.Checksum{}) {
			t.Fatalf("Inspect() = %v, %v", fingerprint, err)
		}
		assertFaultExpectations(t, mock)
	})

	for _, test := range []struct {
		name string
		rows *sqlmock.Rows
		err  error
	}{
		{name: "query", err: errors.New("catalog unavailable")},
		{name: "scan", rows: sqlmock.NewRows([]string{"object_identity", "definition"}).AddRow(nil, "definition")},
		{name: "iteration", rows: sqlmock.NewRows([]string{"object_identity", "definition"}).AddRow("table:x", "definition").RowError(0, errors.New("iteration failed"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, mock := faultDatabase(t)
			backend, err := New(database)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			expectation := mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects")
			if test.err != nil {
				expectation.WillReturnError(test.err)
			} else {
				expectation.WillReturnRows(test.rows)
			}
			if _, err := backend.InspectObjects(context.Background()); err == nil {
				t.Fatal("InspectObjects() error = nil")
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestFingerprintRejectsAmbiguousSchemaObjects(t *testing.T) {
	t.Parallel()

	for _, objects := range [][]SchemaObject{
		{{Identity: "", Definition: "definition"}},
		{{Identity: "table:x", Definition: ""}},
		{{Identity: "table:x", Definition: "one"}, {Identity: "table:x", Definition: "two"}},
	} {
		if _, err := Fingerprint(objects); !errors.Is(err, ErrInvalidSchemaSnapshot) {
			t.Fatalf("Fingerprint(%#v) error = %v", objects, err)
		}
	}
}

func TestConfigurationAndReleasedSessionGuards(t *testing.T) {
	t.Parallel()

	database, _ := faultDatabase(t)
	if _, err := New(database, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(nil option) error = %v", err)
	}
	if _, err := New(database, WithLockRetryInterval(0)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(retry interval) error = %v", err)
	}
	if _, err := New(database, WithLockRetryInterval(time.Millisecond)); err != nil {
		t.Fatalf("New(valid retry interval) error = %v", err)
	}

	released := &session{}
	migration := faultMigration(t, migrations.TransactionModeDefault)
	if err := released.Prepare(context.Background()); !errors.Is(err, ErrSessionReleased) {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := released.Records(context.Background()); !errors.Is(err, ErrSessionReleased) {
		t.Fatalf("Records() error = %v", err)
	}
	if _, err := released.Apply(context.Background(), migration); !errors.Is(err, ErrSessionReleased) {
		t.Fatalf("Apply() error = %v", err)
	}
	if _, err := released.Rollback(context.Background(), migration); !errors.Is(err, ErrSessionReleased) {
		t.Fatalf("Rollback() error = %v", err)
	}
	if _, err := released.Recover(context.Background(), migration, migrations.RecoveryMarkApplied); !errors.Is(err, ErrSessionReleased) {
		t.Fatalf("Recover() error = %v", err)
	}
	fingerprint := migrations.ChecksumData([]byte("schema"))
	baseline, err := migrations.NewBaseline(100, "baseline", fingerprint)
	if err != nil {
		t.Fatalf("NewBaseline() error = %v", err)
	}
	if _, err := released.Baseline(context.Background(), baseline); !errors.Is(err, ErrSessionReleased) {
		t.Fatalf("Baseline() error = %v", err)
	}
}

func TestSessionRecordsCoversSuccessAndDriverFailures(t *testing.T) {
	t.Parallel()

	migration := faultMigration(t, migrations.TransactionModeDefault)
	appliedAt := time.Now().UTC()
	t.Run("success", func(t *testing.T) {
		session, mock := faultSession(t, 0)
		mock.ExpectQuery("SELECT (.+) FROM public.go_schema_migrations").WillReturnRows(
			sqlmock.NewRows([]string{"kind", "version", "name", "checksum", "started_at", "finished_at", "execution_time_ms", "dirty"}).
				AddRow("baseline", 100, "baseline", migration.Checksum().String(), appliedAt, appliedAt, 0, false).
				AddRow("migration", 101, "migration", migration.Checksum().String(), appliedAt, appliedAt, 1, false),
		)
		records, err := session.Records(context.Background())
		if err != nil || len(records) != 2 || records[0].Kind() != migrations.RecordKindBaseline {
			t.Fatalf("Records() = %#v, %v", records, err)
		}
		assertFaultExpectations(t, mock)
	})

	for _, test := range []struct {
		name string
		rows *sqlmock.Rows
		err  error
	}{
		{name: "query", err: errors.New("query failed")},
		{name: "scan", rows: sqlmock.NewRows([]string{"kind"}).AddRow("migration")},
		{name: "iteration", rows: sqlmock.NewRows([]string{"kind", "version", "name", "checksum", "started_at", "finished_at", "execution_time_ms", "dirty"}).AddRow("migration", 1, "migration", migration.Checksum().String(), appliedAt, appliedAt, 1, false).RowError(0, errors.New("rows failed"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, mock := faultSession(t, 0)
			expectation := mock.ExpectQuery("SELECT (.+) FROM public.go_schema_migrations")
			if test.err != nil {
				expectation.WillReturnError(test.err)
			} else {
				expectation.WillReturnRows(test.rows)
			}
			if _, err := session.Records(context.Background()); err == nil {
				t.Fatal("Records() error = nil")
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestRecoveryPersistenceOutcomes(t *testing.T) {
	t.Parallel()

	migration := faultMigration(t, migrations.TransactionModeNone)
	now := time.Now().UTC()
	tests := []struct {
		name   string
		action migrations.RecoveryAction
		rows   *sqlmock.Rows
		err    error
		target error
	}{
		{name: "applied missing", action: migrations.RecoveryMarkApplied, rows: sqlmock.NewRows([]string{"started_at"}), target: migrations.ErrNoDirtyMigration},
		{name: "applied query", action: migrations.RecoveryMarkApplied, err: errors.New("update failed")},
		{name: "applied future start", action: migrations.RecoveryMarkApplied, rows: sqlmock.NewRows([]string{"started_at"}).AddRow(now.Add(time.Hour))},
		{name: "rolled back missing", action: migrations.RecoveryMarkRolledBack, rows: sqlmock.NewRows([]string{"started_at", "execution_time_ms"}), target: migrations.ErrNoDirtyMigration},
		{name: "rolled back query", action: migrations.RecoveryMarkRolledBack, err: errors.New("delete failed")},
		{name: "rolled back success", action: migrations.RecoveryMarkRolledBack, rows: sqlmock.NewRows([]string{"started_at", "execution_time_ms"}).AddRow(now, 12)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, mock := faultSession(t, 0)
			query := "UPDATE public.go_schema_migrations"
			if test.action == migrations.RecoveryMarkRolledBack {
				query = "DELETE FROM public.go_schema_migrations"
			}
			expectation := mock.ExpectQuery(query)
			if test.err != nil {
				expectation.WillReturnError(test.err)
			} else {
				expectation.WillReturnRows(test.rows)
			}
			record, err := session.Recover(context.Background(), migration, test.action)
			if test.err != nil || test.target != nil {
				if err == nil {
					t.Fatal("Recover() error = nil")
				}
				if test.target != nil && !errors.Is(err, test.target) {
					t.Fatalf("Recover() error = %v, want %v", err, test.target)
				}
			} else if err != nil || record.Version() != migration.Version() {
				t.Fatalf("Recover() = %#v, %v", record, err)
			}
			assertFaultExpectations(t, mock)
		})
	}

	session, _ := faultSession(t, 0)
	if _, err := session.Recover(context.Background(), migration, 99); !errors.Is(err, migrations.ErrInvalidRecovery) {
		t.Fatalf("Recover(invalid) error = %v", err)
	}
}

func TestTransactionalRollbackStopsAtEveryPersistenceBoundary(t *testing.T) {
	t.Parallel()

	fault := errors.New("injected rollback boundary fault")
	migration := faultMigration(t, migrations.TransactionModeDefault)
	appliedAt := time.Now().UTC()
	tests := []struct {
		name  string
		setup func(sqlmock.Sqlmock)
	}{
		{name: "begin", setup: func(mock sqlmock.Sqlmock) { mock.ExpectBegin().WillReturnError(fault) }},
		{name: "timeout", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectExec("set_config").WillReturnError(fault)
			mock.ExpectRollback()
		}},
		{name: "statement", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnError(fault)
			mock.ExpectRollback()
		}},
		{name: "ledger delete", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("DELETE FROM public.go_schema_migrations").WillReturnError(fault)
			mock.ExpectRollback()
		}},
		{name: "commit", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("DELETE FROM public.go_schema_migrations").WillReturnRows(
				sqlmock.NewRows([]string{"finished_at", "execution_time_ms"}).AddRow(appliedAt, 1),
			)
			mock.ExpectCommit().WillReturnError(fault)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, mock := faultSession(t, time.Second)
			test.setup(mock)
			if _, err := session.Rollback(context.Background(), migration); err == nil {
				t.Fatal("Rollback() error = nil")
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestNoTransactionApplyPersistenceOutcomes(t *testing.T) {
	t.Parallel()

	migration := faultMigration(t, migrations.TransactionModeNone)
	fault := errors.New("injected no-transaction fault")

	t.Run("timeout setup", func(t *testing.T) {
		session, mock := faultSession(t, time.Second)
		mock.ExpectExec("set_config").WillReturnError(fault)
		if _, err := session.Apply(context.Background(), migration); !errors.Is(err, fault) {
			t.Fatalf("Apply() error = %v", err)
		}
		assertFaultExpectations(t, mock)
	})

	t.Run("dirty insert", func(t *testing.T) {
		session, mock := faultSession(t, 0)
		mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnError(fault)
		if _, err := session.Apply(context.Background(), migration); !errors.Is(err, fault) {
			t.Fatalf("Apply() error = %v", err)
		}
		assertFaultExpectations(t, mock)
	})

	t.Run("clean update", func(t *testing.T) {
		session, mock := faultSession(t, 0)
		mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("UPDATE public.go_schema_migrations").WillReturnError(fault)
		if _, err := session.Apply(context.Background(), migration); !errors.Is(err, fault) {
			t.Fatalf("Apply() error = %v", err)
		}
		assertFaultExpectations(t, mock)
	})

	t.Run("success", func(t *testing.T) {
		session, mock := faultSession(t, 0)
		mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("UPDATE public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
		record, err := session.Apply(context.Background(), migration)
		if err != nil || record.Version() != migration.Version() {
			t.Fatalf("Apply() = %#v, %v", record, err)
		}
		assertFaultExpectations(t, mock)
	})

	t.Run("timeout reset", func(t *testing.T) {
		session, mock := faultSession(t, time.Second)
		mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnError(fault)
		mock.ExpectExec("set_config").WillReturnError(errors.New("reset failed"))
		if _, err := session.Apply(context.Background(), migration); !errors.Is(err, fault) {
			t.Fatalf("Apply() error = %v", err)
		}
		assertFaultExpectations(t, mock)
	})
}

func TestNoTransactionRollbackPersistenceOutcomes(t *testing.T) {
	t.Parallel()

	migration := faultMigration(t, migrations.TransactionModeNone)
	fault := errors.New("injected no-transaction rollback fault")
	appliedAt := time.Now().UTC()
	tests := []struct {
		name  string
		setup func(sqlmock.Sqlmock)
		ok    bool
	}{
		{name: "timeout", setup: func(mock sqlmock.Sqlmock) { mock.ExpectExec("set_config").WillReturnError(fault) }},
		{name: "missing", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("UPDATE public.go_schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"started_at", "execution_time_ms"}))
		}},
		{name: "mark dirty", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("UPDATE public.go_schema_migrations").WillReturnError(fault)
		}},
		{name: "delete", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("UPDATE public.go_schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"started_at", "execution_time_ms"}).AddRow(appliedAt, 1))
			mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("DELETE FROM public.go_schema_migrations").WillReturnError(fault)
		}},
		{name: "delete result", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("UPDATE public.go_schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"started_at", "execution_time_ms"}).AddRow(appliedAt, 1))
			mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("DELETE FROM public.go_schema_migrations").WillReturnResult(sqlmock.NewErrorResult(fault))
		}},
		{name: "conflict", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("UPDATE public.go_schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"started_at", "execution_time_ms"}).AddRow(appliedAt, 1))
			mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("DELETE FROM public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 0))
		}},
		{name: "success", ok: true, setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("UPDATE public.go_schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"started_at", "execution_time_ms"}).AddRow(appliedAt, 1))
			mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("DELETE FROM public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			timeout := time.Duration(0)
			if test.name == "timeout" {
				timeout = time.Second
			}
			session, mock := faultSession(t, timeout)
			test.setup(mock)
			record, err := session.Rollback(context.Background(), migration)
			if test.ok {
				if err != nil || record.Version() != migration.Version() {
					t.Fatalf("Rollback() = %#v, %v", record, err)
				}
			} else if err == nil {
				t.Fatal("Rollback() error = nil")
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestSessionRejectsInvalidAndIrreversibleMigrations(t *testing.T) {
	t.Parallel()

	session, _ := faultSession(t, 0)
	if _, err := session.Apply(context.Background(), migrations.Migration{}); err == nil {
		t.Fatal("Apply(invalid) error = nil")
	}
	if _, err := session.Rollback(context.Background(), migrations.Migration{}); err == nil {
		t.Fatal("Rollback(invalid) error = nil")
	}

	irreversible, err := migrations.NewMigration(1, "irreversible", migrations.TransactionModeDefault, "SELECT 1;", "")
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}
	if _, err := session.Rollback(context.Background(), irreversible); !errors.Is(err, migrations.ErrIrreversible) {
		t.Fatalf("Rollback(irreversible) error = %v", err)
	}
}

func TestDecodeRecordRejectsMalformedPersistedValues(t *testing.T) {
	t.Parallel()

	checksum := migrations.ChecksumData([]byte("migration")).String()
	now := time.Now().UTC()
	for _, test := range []struct {
		name       string
		version    int64
		recordName string
		finishedAt time.Time
		durationMS int64
	}{
		{name: "negative duration", version: 1, recordName: "valid", finishedAt: now, durationMS: -1},
		{name: "invalid name", version: 1, recordName: "Not Valid", finishedAt: now},
		{name: "zero time", version: 1, recordName: "valid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeRecord("migration", test.version, test.recordName, checksum, test.finishedAt, test.durationMS, false); !errors.Is(err, migrations.ErrInvalidRecord) {
				t.Fatalf("decodeRecord() error = %v", err)
			}
		})
	}
}

func TestBaselineStopsAtEveryPersistenceBoundary(t *testing.T) {
	t.Parallel()

	objects := []SchemaObject{{Identity: "table:public.users", Definition: "table definition"}}
	fingerprint, err := Fingerprint(objects)
	if err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	baseline, err := migrations.NewBaseline(100, "baseline", fingerprint)
	if err != nil {
		t.Fatalf("NewBaseline() error = %v", err)
	}
	fault := errors.New("injected baseline fault")
	tests := []struct {
		name  string
		setup func(sqlmock.Sqlmock)
	}{
		{name: "begin", setup: func(mock sqlmock.Sqlmock) { mock.ExpectBegin().WillReturnError(fault) }},
		{name: "inspect", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").WillReturnError(fault)
			mock.ExpectRollback()
		}},
		{name: "drift", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").WillReturnRows(
				sqlmock.NewRows([]string{"object_identity", "definition"}).AddRow("table:public.other", "other definition"),
			)
			mock.ExpectRollback()
		}},
		{name: "insert", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").WillReturnRows(
				sqlmock.NewRows([]string{"object_identity", "definition"}).AddRow(objects[0].Identity, objects[0].Definition),
			)
			mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnError(fault)
			mock.ExpectRollback()
		}},
		{name: "commit", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").WillReturnRows(
				sqlmock.NewRows([]string{"object_identity", "definition"}).AddRow(objects[0].Identity, objects[0].Definition),
			)
			mock.ExpectExec("INSERT INTO public.go_schema_migrations").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit().WillReturnError(fault)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, mock := faultSession(t, 0)
			test.setup(mock)
			if _, err := session.Baseline(context.Background(), baseline); err == nil {
				t.Fatal("Baseline() error = nil")
			}
			assertFaultExpectations(t, mock)
		})
	}
}

func TestSchemaInspectionRejectsClosedDatabase(t *testing.T) {
	t.Parallel()

	database, mock := faultDatabase(t)
	mock.ExpectClose()
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	backend, err := New(database)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := backend.Inspect(context.Background()); err == nil {
		t.Fatal("Inspect(closed) error = nil")
	}
	if _, err := backend.InspectObjects(context.Background()); err == nil {
		t.Fatal("InspectObjects(closed) error = nil")
	}
}

func faultSession(t *testing.T, timeout time.Duration) (*session, sqlmock.Sqlmock) {
	t.Helper()

	database, mock := faultDatabase(t)
	connection, err := database.Conn(context.Background())
	if err != nil {
		t.Fatalf("database.Conn() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	return &session{connection: connection, statementTimeout: timeout}, mock
}

func faultDatabase(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()

	database, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	return database, mock
}

func faultMigration(t *testing.T, mode migrations.TransactionMode) migrations.Migration {
	t.Helper()

	migration, err := migrations.NewMigration(
		1,
		"fault_boundary",
		mode,
		"CREATE TABLE fault_boundary (id bigint);",
		"DROP TABLE fault_boundary;",
	)
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}

	return migration
}

func assertFaultExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}
