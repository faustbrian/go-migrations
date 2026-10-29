//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/faustbrian/go-migrations/v5/postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPgxDiscardRemovesSessionFromPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, _ := isolatedMigrationDatabase(t, ctx)
	pool := clonePool(t, ctx, fixture, 1)

	released, err := postgres.LeasedBackendPID(ctx, pool, false)
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := postgres.LeasedBackendPID(ctx, pool, true)
	if err != nil {
		t.Fatal(err)
	}
	if discarded != released {
		t.Fatalf("released session %d was not reused (got %d)", released, discarded)
	}
	next, err := postgres.LeasedBackendPID(ctx, pool, false)
	if err != nil {
		t.Fatal(err)
	}
	if next == discarded {
		t.Fatalf("discarded session %d reached the next borrower", discarded)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var alive bool
		if err := fixture.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)", discarded).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discarded server session %d is still running", discarded)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestPgxExecutesMultiStatementMigrationsInBothModes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	backend, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	session, err := backend.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := session.Release(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := session.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	transactional, err := migrations.NewMigration(1, "transactional_batch", migrations.TransactionModeDefault,
		"CREATE TABLE public.multi_tx(id int PRIMARY KEY);\nINSERT INTO public.multi_tx VALUES (1);\nINSERT INTO public.multi_tx VALUES (2);\n",
		"DROP TABLE public.multi_tx;\nSELECT 1;\n")
	if err != nil {
		t.Fatal(err)
	}
	direct, err := migrations.NewMigration(2, "direct_batch", migrations.TransactionModeNone,
		"CREATE TABLE public.multi_direct(id int);\nINSERT INTO public.multi_direct VALUES (3);\n",
		"DROP TABLE public.multi_direct;\nSELECT 2;\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []migrations.Migration{transactional, direct} {
		if _, err := session.Apply(ctx, migration); err != nil {
			t.Fatalf("Apply(%s) error = %v", migration.Name(), err)
		}
	}
	var rows int
	if err := db.QueryRow(ctx, "SELECT (SELECT count(*) FROM public.multi_tx) + (SELECT count(*) FROM public.multi_direct)").Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("multi-statement effects = %d, %v; want 3 rows", rows, err)
	}
	for _, migration := range []migrations.Migration{direct, transactional} {
		if _, err := session.Rollback(ctx, migration); err != nil {
			t.Fatalf("Rollback(%s) error = %v", migration.Name(), err)
		}
	}
	var absent bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.multi_tx') IS NULL AND to_regclass('public.multi_direct') IS NULL").Scan(&absent); err != nil || !absent {
		t.Fatalf("multi-statement down SQL did not run: %v", err)
	}
}

// Caller cancellation interrupts the statement and pgx closes the connection.
// The transaction can no longer commit, so the result must stay a pure
// context failure rather than gaining a synthetic rollback failure.
func TestPgxCanceledTransactionalApplyStaysPureCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	backend, err := postgres.New(db, postgres.WithLockTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	session, err := backend.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	slow, err := migrations.NewMigration(1, "slow_apply", migrations.TransactionModeDefault,
		"CREATE TABLE public.slow_apply(id int);\nSELECT pg_sleep(10);\n", "")
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = session.Apply(short, slow)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, migrations.ErrExecutionFailed) || errors.Is(err, postgres.ErrDatabaseOperationFailed) {
		t.Fatalf("Apply() error = %v, want redacted execution deadline without cleanup failure", err)
	}
	if err := session.Release(context.Background()); err != nil {
		t.Fatalf("Release() after cancellation error = %v, want nil", err)
	}

	next, err := backend.Acquire(ctx)
	if err != nil {
		t.Fatalf("lock was not freed by the closed session: %v", err)
	}
	defer func() {
		if err := next.Release(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	records, err := next.Records(ctx)
	if err != nil || len(records) != 0 {
		t.Fatalf("interrupted transaction left history %v, %v", records, err)
	}
	var absent bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.slow_apply') IS NULL").Scan(&absent); err != nil || !absent {
		t.Fatalf("interrupted transaction left schema effects: %v", err)
	}
}

// Cancelling an in-flight no-transaction statement must leave the dirty row
// for operator recovery, keep the context classification, end the
// interrupted server session instead of pooling it, and free the lock.
func TestPgxCanceledNoTransactionStatementStaysDirtyAndDiscardsSession(t *testing.T) {
	for _, down := range []bool{false, true} {
		name := "apply"
		if down {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db, _ := isolatedMigrationDatabase(t, ctx)
			backend, err := postgres.New(db, postgres.WithLockTimeout(5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			upSQL, downSQL := "SELECT pg_sleep(10);", ""
			if down {
				upSQL, downSQL = "CREATE TABLE public.direct_cancel(id int);", "SELECT pg_sleep(10);"
			}
			migration, err := migrations.NewMigration(1, "direct_cancel", migrations.TransactionModeNone, upSQL, downSQL)
			if err != nil {
				t.Fatal(err)
			}
			session, err := backend.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if down {
				if _, err := session.Apply(ctx, migration); err != nil {
					t.Fatal(err)
				}
			}
			var pid int32
			if err := db.QueryRow(ctx, "SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())").Scan(&pid); err != nil {
				t.Fatalf("lock owner: %v", err)
			}
			short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
			if down {
				_, err = session.Rollback(short, migration)
			} else {
				_, err = session.Apply(short, migration)
			}
			stop()
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, migrations.ErrExecutionFailed) || errors.Is(err, postgres.ErrDatabaseOperationFailed) {
				t.Fatalf("interrupted statement error = %v, want pure redacted execution deadline", err)
			}
			if err := session.Release(context.Background()); err != nil {
				t.Fatalf("Release() after cancellation error = %v, want nil", err)
			}

			deadline := time.Now().Add(10 * time.Second)
			for {
				var alive bool
				if err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)", pid).Scan(&alive); err != nil {
					t.Fatal(err)
				}
				if !alive {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("interrupted session %d was not discarded", pid)
				}
				time.Sleep(25 * time.Millisecond)
			}
			next, err := backend.Acquire(ctx)
			if err != nil {
				t.Fatalf("lock was not freed: %v", err)
			}
			defer func() {
				if err := next.Release(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			records, err := next.Records(ctx)
			if err != nil || len(records) != 1 || !records[0].Dirty() || records[0].Checksum() != migration.Checksum() {
				t.Fatalf("interrupted statement history = %v, %v; want one dirty row", records, err)
			}
		})
	}
}

// Cancelling transactional down SQL must roll back the delete and leave the
// clean row, report only the redacted deadline and release cleanly.
func TestPgxCanceledTransactionalRollbackKeepsCleanHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	backend, err := postgres.New(db, postgres.WithLockTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	migration, err := migrations.NewMigration(1, "slow_down", migrations.TransactionModeDefault,
		"CREATE TABLE public.slow_down(id int);", "DROP TABLE public.slow_down;\nSELECT pg_sleep(10);\n")
	if err != nil {
		t.Fatal(err)
	}
	session, err := backend.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Apply(ctx, migration); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = session.Rollback(short, migration)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, migrations.ErrExecutionFailed) || errors.Is(err, postgres.ErrDatabaseOperationFailed) {
		t.Fatalf("Rollback() error = %v, want pure redacted execution deadline", err)
	}
	if err := session.Release(context.Background()); err != nil {
		t.Fatalf("Release() after cancellation error = %v, want nil", err)
	}
	next, err := backend.Acquire(ctx)
	if err != nil {
		t.Fatalf("lock was not freed: %v", err)
	}
	defer func() {
		if err := next.Release(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	records, err := next.Records(ctx)
	if err != nil || len(records) != 1 || records[0].Dirty() {
		t.Fatalf("interrupted rollback history = %v, %v; want one clean row", records, err)
	}
	var present bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.slow_down') IS NOT NULL").Scan(&present); err != nil || !present {
		t.Fatalf("interrupted rollback dropped the table: %v", err)
	}
}

// With PostgreSQL's cancel-request handler the session survives cancellation:
// cleanup must still restore statement_timeout, unlock and return the session
// to the pool rather than discard it.
func TestPgxCancelRequestKeepsSessionAndRestoresIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture, _ := isolatedMigrationDatabase(t, ctx)
	config := fixture.Config()
	config.MaxConns = 1
	config.ConnConfig.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: 5 * time.Second}
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	backend, err := postgres.New(pool, postgres.WithLockTimeout(5*time.Second), postgres.WithStatementTimeout(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	migration, err := migrations.NewMigration(1, "cancel_request", migrations.TransactionModeNone, "SELECT pg_sleep(10);", "")
	if err != nil {
		t.Fatal(err)
	}
	session, err := backend.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := fixture.QueryRow(ctx, "SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())").Scan(&pid); err != nil {
		t.Fatalf("lock owner: %v", err)
	}
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = session.Apply(short, migration)
	stop()
	if !errors.Is(err, migrations.ErrExecutionFailed) || errors.Is(err, postgres.ErrDatabaseOperationFailed) {
		t.Fatalf("Apply() error = %v, want redacted execution failure without cleanup failure", err)
	}
	if err := session.Release(context.Background()); err != nil {
		t.Fatalf("Release() after cancel request error = %v, want nil", err)
	}
	var reusedPID int32
	var timeout string
	var locks int
	err = pool.QueryRow(ctx, "SELECT pg_backend_pid(), current_setting('statement_timeout'), (SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid())").Scan(&reusedPID, &timeout, &locks)
	if err != nil {
		t.Fatal(err)
	}
	if reusedPID != pid || timeout != "0" || locks != 0 {
		t.Fatalf("surviving session pid=%d (owner %d) statement_timeout=%s advisory locks=%d; want reused, restored and unlocked", reusedPID, pid, timeout, locks)
	}
}
