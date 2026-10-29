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

func TestCommandRollbackFailureDiscardsOwnership(t *testing.T) {
	for _, name := range []string{"batch", "reservation", "discovery"} {
		t.Run(name, func(t *testing.T) {
			reservation, discovery := name == "reservation", name == "discovery"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			db, _ := isolatedMigrationDatabase(t, ctx)
			fault := &postgres.CommandFault{Checkpoint: !reservation && !discovery, Rollback: true, FailReservation: reservation}
			fault.RollbackArmed.Store(discovery)
			backend, err := postgres.NewFaultBackend(clonePool(t, ctx, db, 0), fault)
			if err != nil {
				t.Fatal(err)
			}
			step, err := migrations.NewMigration(1, "rollback_command", migrations.TransactionModeDefault, "CREATE TABLE public.rollback_command(id int PRIMARY KEY); INSERT INTO public.rollback_command VALUES(1)", "")
			if err != nil {
				t.Fatal(err)
			}
			req := postgres.CommandRequest{ID: "rollback_command_0001", Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{2}, Histories: []postgres.CommandHistory{{Namespace: "rollback", Source: []migrations.Migration{step}}}, Batches: []postgres.CommandBatch{{Namespace: "rollback", Migrations: []migrations.Migration{step}}}}
			owner, err := backend.AcquireCommand(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { releaseCommandFixture(owner) })
			_, executeErr := owner.Execute(ctx, req)
			_, _, readErr := owner.ReadProgress(ctx, req.ID)
			releaseErr := owner.Release(ctx)
			if errors.Is(executeErr, postgres.ErrCommandOutcomeUnknown) == discovery || !errors.Is(executeErr, postgres.ErrDatabaseOperationFailed) || !errors.Is(readErr, postgres.ErrSessionReleased) || releaseErr != nil || fault.Discarded.Load() != 1 {
				t.Fatal("uncertain rollback remained reusable or pooled", executeErr, readErr, releaseErr, fault.Discarded.Load())
			}
			var untouched bool
			if err := db.QueryRow(ctx, "SELECT to_regclass('public.rollback_command') IS NULL AND to_regclass('public.golib_migrations_rollback') IS NULL").Scan(&untouched); err != nil || !untouched {
				t.Fatal("discard failed to rollback server transaction", err)
			}
			clean, err := postgres.New(db)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := clean.AcquireCommand(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseCommandFixture(recovered)
			_, exists, err := recovered.ReadProgress(ctx, req.ID)
			if err != nil || exists != (!reservation && !discovery) {
				t.Fatal("rollback changed original reservation outcome", err)
			}
			progress, err := recovered.Execute(ctx, req)
			if err != nil || !progress.Complete() || len(progress.Receipts()) != 1 {
				t.Fatal("discarded ownership did not permit reconciled recovery", err)
			}
		})
	}
}

func TestCommandCheckpointFailureRollsBackEffectsAndLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	backend, err := postgres.NewFaultBackend(clonePool(t, ctx, db, 0), &postgres.CommandFault{Checkpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	step, err := migrations.NewMigration(1, "checkpoint_command", migrations.TransactionModeDefault, "CREATE TABLE public.checkpoint_command(id int PRIMARY KEY); INSERT INTO public.checkpoint_command VALUES(1)", "")
	if err != nil {
		t.Fatal(err)
	}
	req := postgres.CommandRequest{ID: "checkpoint_command_0001", Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{2}, Histories: []postgres.CommandHistory{{Namespace: "checkpoint", Source: []migrations.Migration{step}}}, Batches: []postgres.CommandBatch{{Namespace: "checkpoint", Migrations: []migrations.Migration{step}}}}
	owner, err := backend.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseCommandFixture(owner) })
	progress, err := owner.Execute(ctx, req)
	if err == nil || progress.Complete() || len(progress.Receipts()) != 0 {
		t.Fatal("failed checkpoint acknowledged migration", err)
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.checkpoint_command') IS NULL AND to_regclass('public.golib_migrations_checkpoint') IS NULL").Scan(&untouched); err != nil || !untouched {
		t.Fatal("checkpoint failure retained SQL/native ledger", err)
	}
	clean, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := clean.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseCommandFixture(recovered)
	stored, exists, err := recovered.ReadProgress(ctx, req.ID)
	if err != nil || !exists || stored.Complete() || len(stored.Receipts()) != 0 {
		t.Fatal("failed checkpoint retained attributed prefix", err)
	}
	result, err := recovered.Execute(ctx, req)
	if err != nil || !result.Complete() || len(result.Receipts()) != 1 {
		t.Fatal("rolled-back checkpoint could not resume once", err)
	}
}

func TestCommandUnknownCommitReconciliation(t *testing.T) {
	for _, before := range []bool{false, true} {
		name := "committed_ack_lost"
		if before {
			name = "rolled_back_before_commit"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			db, _ := isolatedMigrationDatabase(t, ctx)
			backend, err := postgres.NewFaultBackend(clonePool(t, ctx, db, 1), &postgres.CommandFault{Before: before})
			if err != nil {
				t.Fatal(err)
			}
			step, err := migrations.NewMigration(1, "unknown_command", migrations.TransactionModeDefault, "CREATE TABLE public.unknown_command(id int PRIMARY KEY); INSERT INTO public.unknown_command VALUES(1)", "")
			if err != nil {
				t.Fatal(err)
			}
			req := postgres.CommandRequest{ID: "migration_unknown_0001", Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{2}, Histories: []postgres.CommandHistory{{Namespace: "unknown", Source: []migrations.Migration{step}}}, Batches: []postgres.CommandBatch{{Namespace: "unknown", Migrations: []migrations.Migration{step}}}}
			owner, err := backend.AcquireCommand(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { releaseCommandFixture(owner) })
			progress, err := owner.Execute(ctx, req)
			if !errors.Is(err, postgres.ErrCommandOutcomeUnknown) || progress.Complete() {
				t.Fatal("lost commit acknowledgement inferred success/rollback", err)
			}
			if err := owner.Release(ctx); err != nil {
				t.Fatal(err)
			}
			var effect bool
			if err := db.QueryRow(ctx, "SELECT to_regclass('public.unknown_command') IS NOT NULL").Scan(&effect); err != nil || effect == before {
				t.Fatal("fault did not establish intended server outcome", err)
			}
			clean, err := postgres.New(db)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := clean.AcquireCommand(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseCommandFixture(recovered)
			stored, exists, err := recovered.ReadProgress(ctx, req.ID)
			want := 1
			if before {
				want = 0
			}
			if err != nil || !exists || len(stored.Receipts()) != want || stored.Complete() == before {
				t.Fatal("native attribution does not prove server outcome", err)
			}
			result, err := recovered.Execute(ctx, req)
			if err != nil || !result.Complete() || len(result.Receipts()) != 1 {
				t.Fatal("authoritatively reconciled prefix did not finish once", err)
			}
		})
	}
}
