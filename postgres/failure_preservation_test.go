package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/faustbrian/go-migrations/v5/postgres"
	"github.com/jackc/pgx/v5"
)

func TestAcquirePreservesQueryFailureDuringCancellation(t *testing.T) {
	t.Parallel()
	independent := errors.New("private lock query failure")
	for name, test := range map[string]struct {
		cause  error
		cancel bool
	}{
		"caller cancellation":        {context.Canceled, true},
		"dependency deadline":        {context.DeadlineExceeded, false},
		"deadline with cancellation": {context.DeadlineExceeded, true},
		"database failure":           {independent, false},
		"coincident cancellation":    {independent, true},
		"joined cancellation":        {errors.Join(context.Canceled, independent), true},
		"reversed join":              {errors.Join(independent, context.Canceled), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			database, mock := newMockDatabase(t)
			database.OnQuery = func(string) {
				if test.cancel {
					cancel()
				}
			}
			mock.ExpectQuery("pg_try_advisory_lock").WillReturnError(test.cause)
			mock.ExpectClose()
			backend, err := postgres.NewMockBackend(database)
			if err != nil {
				t.Fatal(err)
			}
			session, err := backend.Acquire(ctx)
			if session != nil || !errors.Is(err, test.cause) || database.Discarded != 1 || database.Released != 0 {
				t.Fatalf("acquisition lost failure or retained physical ownership: %v, discarded=%d released=%d", err, database.Discarded, database.Released)
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
			for _, cleanup := range []error{nil, pgx.ErrTxClosed, cleanupFailure} {
				t.Run(fmt.Sprintf("down=%t/statement=%t/cleanup=%v", down, statement, cleanup), func(t *testing.T) {
					t.Parallel()
					database, mock := newMockDatabase(t)
					session := acquireSession(t, database, mock)
					migration := newMigration(t, migrations.TransactionModeDefault)
					mock.ExpectBegin()
					if statement {
						mock.ExpectExec("set_config").WillReturnResult(postgres.MockResult("SELECT", 1))
						if !down {
							mock.ExpectExec("INSERT INTO public.migrations").WillReturnResult(postgres.MockResult("SELECT", 1))
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
					if !errors.Is(err, original) || errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, cleanupFailure) != (cleanup == cleanupFailure) {
						t.Fatalf("operation/cleanup causes were not preserved: %v", err)
					}
					assertPrivateFailureRedacted(t, err)
					mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(postgres.NewMockRows([]string{"unlocked"}).AddRow(true))
					if cleanup == cleanupFailure {
						mock.ExpectClose()
					}
					if err := session.Release(context.Background()); err != nil {
						t.Fatal(err)
					}
					if discarded := database.Discarded == 1; discarded != (cleanup == cleanupFailure) || database.Released+database.Discarded != 1 {
						t.Fatalf("uncertain rollback discarded=%d released=%d", database.Discarded, database.Released)
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
