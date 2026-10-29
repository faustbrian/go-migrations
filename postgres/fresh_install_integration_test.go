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

// MIG-02: database DDL and cluster-level role reservations are both
// transactional. A failure after the first step must leave neither behind.
func TestInstallFreshAtomicPostgreSQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	backend, err := postgres.New(db, postgres.WithOperationTimeout(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first, err := migrations.NewMigration(1, "fresh_first", migrations.TransactionModeDefault, "CREATE TABLE public.fresh_first(id bigint PRIMARY KEY)", "")
	if err != nil {
		t.Fatal(err)
	}
	failing, err := migrations.NewMigration(2, "fresh_second", migrations.TransactionModeDefault, "SELECT 1/0", "")
	if err != nil {
		t.Fatal(err)
	}
	// Derive a safe cluster-unique role from the per-test random database name.
	var databaseName string
	if err := db.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatal("cannot identify isolated fixture")
	}
	role := databaseName + "_reserved"
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := db.Exec(cleanup, `DROP ROLE IF EXISTS "`+role+`"`); err != nil {
			t.Error("role cleanup failed")
		}
	})
	preflight := `CREATE ROLE "` + role + `" NOLOGIN`
	result, err := backend.InstallFresh(ctx, []migrations.Migration{first, failing}, preflight)
	if !errors.Is(err, migrations.ErrExecutionFailed) || len(result) != 0 {
		t.Fatal("late failure escaped atomic installation", err)
	}
	var ledger, table, reserved bool
	inspect := func() {
		t.Helper()
		if err := db.QueryRow(ctx, "SELECT to_regclass('public.migrations') IS NOT NULL, to_regclass('public.fresh_first') IS NOT NULL, EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&ledger, &table, &reserved); err != nil {
			t.Fatal("cannot inspect install effects")
		}
	}
	inspect()
	if ledger || table || reserved {
		t.Fatal("failed installation retained ledger, schema or reserved role")
	}
	second, err := migrations.NewMigration(2, "fresh_second", migrations.TransactionModeDefault, "ALTER TABLE public.fresh_first ADD COLUMN value text", "")
	if err != nil {
		t.Fatal(err)
	}
	history := []migrations.Migration{first, second}
	result, err = backend.InstallFresh(ctx, history, preflight)
	if err != nil || len(result) != 2 {
		t.Fatal("complete fresh installation failed", err)
	}
	inspect()
	if !ledger || !table || !reserved {
		t.Fatal("successful installation omitted owned effects")
	}
	stored, err := backend.ReadLedger(ctx)
	if err != nil || len(stored) != 2 {
		t.Fatal("native ledger did not retain complete source", err)
	}
	for i, record := range stored {
		if record.Version() != history[i].Version() || record.Name() != history[i].Name() || record.Checksum() != history[i].Checksum() || record.Dirty() {
			t.Fatal("atomic install changed native identity")
		}
	}
	// A reconstructed owner denies replay before executing the now-conflicting
	// CREATE ROLE. It must classify existing history, not a provider SQL error.
	reconstructed, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	result, err = reconstructed.InstallFresh(ctx, history, preflight)
	if !errors.Is(err, postgres.ErrLedgerConflict) || len(result) != 0 {
		t.Fatal("existing installation was replayed", err)
	}
	if err := db.Ping(ctx); err != nil {
		t.Fatal("installer closed borrowed database")
	}
}
