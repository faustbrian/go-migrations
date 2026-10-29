//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/faustbrian/go-migrations/v5/postgres"
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
	_ = session.Release(context.Background())

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
