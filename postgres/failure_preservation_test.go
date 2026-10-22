package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	migrations "github.com/faustbrian/go-migrations/v3"
	"github.com/faustbrian/go-migrations/v3/postgres"
)

func TestAcquirePreservesQueryFailureDuringCancellation(t *testing.T) {
	t.Parallel()
	independent := errors.New("private lock query failure")
	for name, test := range map[string]struct {
		cause  error
		cancel bool
	}{
		"caller cancellation":     {context.Canceled, true},
		"dependency deadline":     {context.DeadlineExceeded, false},
		"database failure":        {independent, false},
		"coincident cancellation": {independent, true},
		"joined cancellation":     {errors.Join(context.Canceled, independent), true},
		"reversed join":           {errors.Join(independent, context.Canceled), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			connection := &lockFailureConnection{cause: test.cause, interrupt: func() {
				if test.cancel {
					cancel()
				}
			}}
			database := sql.OpenDB(lockFailureConnector{connection})
			t.Cleanup(func() { _ = database.Close() })
			backend, err := postgres.New(database)
			if err != nil {
				t.Fatal(err)
			}
			session, err := backend.Acquire(ctx)
			if session != nil || !errors.Is(err, test.cause) || !connection.closed {
				t.Fatalf("acquisition lost failure or retained physical ownership: %v, closed=%t", err, connection.closed)
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation was lost")
			}
			assertPrivateFailureRedacted(t, err)
		})
	}
}

func TestTransactionalOperationsPreserveExplicitRollbackFailure(t *testing.T) {
	t.Parallel()
	operationFailure := errors.New("private statement failure")
	cleanupFailure := errors.New("private rollback failure")
	for _, down := range []bool{false, true} {
		for _, statement := range []bool{false, true} {
			for _, cleanup := range []error{nil, sql.ErrTxDone, cleanupFailure} {
				t.Run(fmt.Sprintf("down=%t/statement=%t/cleanup=%v", down, statement, cleanup), func(t *testing.T) {
					t.Parallel()
					database, mock := newMockDatabase(t)
					session := acquireSession(t, database, mock)
					migration := newMigration(t, migrations.TransactionModeDefault)
					mock.ExpectBegin()
					if statement {
						mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
						if !down {
							mock.ExpectExec("INSERT INTO public.migrations").WillReturnResult(sqlmock.NewResult(0, 1))
						}
						query := "CREATE TABLE users"
						if down {
							query = "DROP TABLE users"
						}
						mock.ExpectExec(query).WillReturnError(operationFailure)
					} else {
						mock.ExpectExec("set_config").WillReturnError(operationFailure)
					}
					rollback := mock.ExpectRollback()
					if cleanup != nil {
						rollback.WillReturnError(cleanup)
					}
					var err error
					if down {
						_, err = session.Rollback(context.Background(), migration)
					} else {
						_, err = session.Apply(context.Background(), migration)
					}
					original := operationFailure
					if statement {
						original = migrations.ErrExecutionFailed
					}
					if !errors.Is(err, original) || errors.Is(err, sql.ErrTxDone) || errors.Is(err, cleanupFailure) != (cleanup == cleanupFailure) {
						t.Fatalf("operation/cleanup causes were not preserved: %v", err)
					}
					assertPrivateFailureRedacted(t, err)
					mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
					if cleanup == cleanupFailure {
						mock.ExpectClose()
					}
					if err := session.Release(context.Background()); err != nil {
						t.Fatal(err)
					}
					assertExpectations(t, mock)
				})
			}
		}
	}
}

func assertPrivateFailureRedacted(t *testing.T, err error) {
	t.Helper()
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, err), "private") {
			t.Fatalf("format %s disclosed a driver diagnostic", format)
		}
	}
}

type lockFailureConnector struct{ connection *lockFailureConnection }

func (c lockFailureConnector) Connect(context.Context) (driver.Conn, error) { return c.connection, nil }
func (lockFailureConnector) Driver() driver.Driver                          { return lockFailureDriver{} }

type lockFailureDriver struct{}

func (lockFailureDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector required")
}

type lockFailureConnection struct {
	cause     error
	interrupt func()
	closed    bool
}

func (*lockFailureConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *lockFailureConnection) Close() error { c.closed = true; return nil }
func (*lockFailureConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (c *lockFailureConnection) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.interrupt()
	return nil, c.cause
}
