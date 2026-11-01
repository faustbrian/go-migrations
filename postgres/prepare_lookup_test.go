package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	migrations "github.com/faustbrian/go-migrations/v4"
	"github.com/faustbrian/go-migrations/v4/postgres"
)

func TestSessionPrepareUsesOnlySelectedExistingTable(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{"", "restricted"} {
		for _, kind := range []string{"r", "p", "v", "m", "S"} {
			t.Run(namespace+"/"+kind, func(t *testing.T) {
				t.Parallel()
				database, mock := newMockDatabase(t)
				backend, err := postgres.New(database)
				if err != nil {
					t.Fatal(err)
				}
				name := "migrations"
				if namespace != "" {
					backend, err = backend.ForNamespace(namespace)
					if err != nil {
						t.Fatal(err)
					}
					name = "golib_migrations_" + namespace
				}
				mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
				session, err := backend.Acquire(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				mock.ExpectQuery("pg_catalog.pg_class").WithArgs(name).
					WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow(kind))
				err = session.Prepare(context.Background())
				if kind == "r" || kind == "p" {
					if err != nil {
						t.Fatal("existing selected table requires no schema creation", err)
					}
				} else if !errors.Is(err, postgres.ErrLedgerConflict) {
					t.Fatal("non-table selected ledger must conflict", err)
				}
				mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
				if err := session.Release(context.Background()); err != nil {
					t.Fatal(err)
				}
				assertExpectations(t, mock)
			})
		}
	}
}

func TestSessionPreparationLookupHasOperationDeadline(t *testing.T) {
	t.Parallel()
	database, mock := newMockDatabase(t)
	backend, err := postgres.New(database, postgres.WithOperationTimeout(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	session, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("pg_catalog.pg_class").WithArgs("migrations").WillDelayFor(time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow("r"))
	if err := session.Prepare(context.Background()); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, postgres.ErrDatabaseOperationFailed) {
		t.Fatal("catalog lookup lost operation deadline or database failure", err)
	}
	mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
	if err := session.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertExpectations(t, mock)
}

func TestRunnerPreparationRetainsCatalogAndCleanupFailures(t *testing.T) {
	t.Parallel()
	independent := errors.New("private catalog failure")
	cleanup := errors.New("private unlock failure")
	for name, test := range map[string]struct {
		cause   error
		cancel  bool
		cleanup error
	}{
		"database failure":        {cause: independent},
		"caller cancellation":     {cause: context.Canceled, cancel: true},
		"dependency deadline":     {cause: context.DeadlineExceeded},
		"coincident cancellation": {cause: independent, cancel: true},
		"joined cancellation":     {cause: errors.Join(context.Canceled, independent), cancel: true},
		"independent cleanup":     {cause: independent, cancel: true, cleanup: cleanup},
		"mixed missing result":    {cause: errors.Join(sql.ErrNoRows, independent)},
		"mixed missing cleanup":   {cause: errors.Join(sql.ErrNoRows, independent), cleanup: cleanup},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			connection := &prepareFailureConnection{
				lockFailureConnection: &lockFailureConnection{cause: test.cause, interrupt: func() {
					if test.cancel {
						cancel()
					}
				}},
				cleanup: test.cleanup,
			}
			database := sql.OpenDB(prepareFailureConnector{connection})
			t.Cleanup(func() { _ = database.Close() })
			backend, err := postgres.New(database)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := migrations.NewRunner(prepareFailureSource{newMigration(t, migrations.TransactionModeDefault)}, backend)
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.Up(ctx)
			if !errors.Is(err, test.cause) || !errors.Is(err, postgres.ErrDatabaseOperationFailed) ||
				errors.Is(err, cleanup) != (test.cleanup != nil) || (test.cancel && !errors.Is(err, context.Canceled)) {
				t.Fatal("runner lost catalog, cancellation or cleanup cause", err)
			}
			if connection.lookups != 1 || connection.executions != 0 || connection.unlocks != 1 || connection.closed != (test.cleanup != nil) {
				t.Fatalf("lookup failure performed SQL or retained uncertain ownership: lookups=%d executions=%d unlocks=%d closed=%t", connection.lookups, connection.executions, connection.unlocks, connection.closed)
			}
			assertPrivateFailureRedacted(t, err)
		})
	}
}

func TestReadLedgerPreservesMixedCatalogFailure(t *testing.T) {
	t.Parallel()
	independent := errors.New("private catalog failure")
	cleanup := errors.New("private rollback failure")
	for _, rollbackFailure := range []error{nil, cleanup} {
		t.Run("rollback="+fmt.Sprint(rollbackFailure != nil), func(t *testing.T) {
			t.Parallel()
			database, mock := newMockDatabase(t)
			backend, err := postgres.New(database)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			mock.ExpectQuery("pg_catalog.pg_class").WillReturnError(errors.Join(sql.ErrNoRows, independent))
			rollback := mock.ExpectRollback()
			if rollbackFailure != nil {
				rollback.WillReturnError(rollbackFailure)
			}
			records, err := backend.ReadLedger(context.Background())
			if records != nil || !errors.Is(err, independent) || !errors.Is(err, postgres.ErrDatabaseOperationFailed) ||
				errors.Is(err, cleanup) != (rollbackFailure != nil) {
				t.Fatal("discovery hid catalog failure or cleanup failure as empty history", err)
			}
			assertPrivateFailureRedacted(t, err)
			assertExpectations(t, mock)
		})
	}
}

type prepareFailureSource []migrations.Migration

func (source prepareFailureSource) Load(context.Context) ([]migrations.Migration, error) {
	return source, nil
}

type prepareFailureConnector struct{ connection *prepareFailureConnection }

func (connector prepareFailureConnector) Connect(context.Context) (driver.Conn, error) {
	return connector.connection, nil
}

func (prepareFailureConnector) Driver() driver.Driver { return lockFailureDriver{} }

type prepareFailureConnection struct {
	*lockFailureConnection
	cleanup    error
	lookups    int
	executions int
	unlocks    int
}

func (connection *prepareFailureConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "pg_try_advisory_lock") {
		return &prepareBooleanRows{}, nil
	}
	if strings.Contains(query, "pg_advisory_unlock") {
		connection.unlocks++
		if connection.cleanup != nil {
			return nil, connection.cleanup
		}
		return &prepareBooleanRows{}, nil
	}
	connection.lookups++
	return connection.lockFailureConnection.QueryContext(ctx, query, args)
}

func (connection *prepareFailureConnection) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	connection.executions++
	return nil, errors.New("unexpected SQL execution")
}

type prepareBooleanRows struct{ consumed bool }

func (*prepareBooleanRows) Columns() []string { return []string{"locked"} }
func (*prepareBooleanRows) Close() error      { return nil }
func (rows *prepareBooleanRows) Next(values []driver.Value) error {
	if rows.consumed {
		return io.EOF
	}
	rows.consumed = true
	values[0] = true
	return nil
}
