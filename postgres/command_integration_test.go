//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v3"
	"github.com/faustbrian/go-migrations/v3/postgres"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestCommandAtomicPrefixAndNamespaces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	db.SetMaxOpenConns(1)
	contenderDB := stdlib.OpenDB(*commandFixturePGXConfig(t, ctx, db))
	t.Cleanup(func() { _ = contenderDB.Close() })
	backend, err := postgres.New(db, postgres.WithOperationTimeout(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first, err := migrations.NewMigration(1, "command_first", migrations.TransactionModeDefault, "CREATE TABLE public.command_first(id int PRIMARY KEY); INSERT INTO public.command_first VALUES(1)", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := migrations.NewMigration(1, "command_second", migrations.TransactionModeDefault, "CREATE TABLE public.command_second(id int PRIMARY KEY); INSERT INTO public.command_second SELECT id FROM public.command_dependency", "")
	if err != nil {
		t.Fatal(err)
	}
	req := postgres.CommandRequest{ID: "migration_command_0001", Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{11}, Histories: []postgres.CommandHistory{
		{Namespace: "first", Source: []migrations.Migration{first}},
		{Namespace: "second", Source: []migrations.Migration{second}},
	}, Batches: []postgres.CommandBatch{
		{Namespace: "first", Migrations: []migrations.Migration{first}},
		{Namespace: "second", Migrations: []migrations.Migration{second}},
	}}
	owner, err := backend.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = owner.Release(cleanup)
	})
	for _, namespace := range []string{"first", "second"} {
		if records, err := owner.ReadLedger(ctx, namespace); err != nil || len(records) != 0 {
			t.Fatal("owned discovery prepares or deadlocks", err)
		}
	}
	presence, err := owner.CatalogPresence(ctx, []postgres.CatalogIdentity{{Kind: postgres.CatalogSchema, Schema: "public"}, {Kind: postgres.CatalogRelation, Schema: "public", Name: "command_first"}})
	if err != nil || len(presence) != 2 || !presence[0] || presence[1] {
		t.Fatal("owned catalog discovery mutated or misprojected input", err)
	}
	if _, exists, err := owner.ReadProgress(ctx, req.ID); err != nil || exists {
		t.Fatal("missing command was initialized by discovery", err)
	}
	progress, err := owner.Execute(ctx, req)
	if !errors.Is(err, migrations.ErrExecutionFailed) || progress.Complete() {
		t.Fatal("failing second batch succeeded", err)
	}
	stored, exists, err := owner.ReadProgress(ctx, req.ID)
	if err != nil || !exists || len(stored.Receipts()) != 1 || stored.Complete() {
		t.Fatal("committed prefix lost command attribution", err)
	}
	contender, err := postgres.New(contenderDB, postgres.WithLockTimeout(20*time.Millisecond), postgres.WithLockRetryInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if competing, err := contender.AcquireCommand(ctx); !errors.Is(err, context.DeadlineExceeded) || competing != nil {
		t.Fatal("namespace transition released global ownership", err)
	}
	firstRecords, err := owner.ReadLedger(ctx, "first")
	if err != nil || len(firstRecords) != 1 {
		t.Fatal("checkpoint and native record differ", err)
	}
	receipt := stored.Receipts()[0].Record
	if receipt.Kind() != firstRecords[0].Kind() || receipt.Version() != firstRecords[0].Version() || receipt.Name() != firstRecords[0].Name() || receipt.Checksum() != firstRecords[0].Checksum() || !receipt.AppliedAt().Equal(firstRecords[0].AppliedAt()) || receipt.Duration() != firstRecords[0].Duration() || receipt.Dirty() != firstRecords[0].Dirty() {
		t.Fatal("checkpoint changed canonical native record metadata")
	}
	if rows, err := owner.ReadLedger(ctx, "second"); err != nil || len(rows) != 0 {
		t.Fatal("failed batch retained history", err)
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	var firstExists, secondExists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.command_first') IS NOT NULL, to_regclass('public.command_second') IS NOT NULL").Scan(&firstExists, &secondExists); err != nil || !firstExists || secondExists {
		t.Fatal("SQL effects did not match atomic prefix", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE public.command_dependency(id int PRIMARY KEY); INSERT INTO public.command_dependency VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	// Reconstruct both owner and command execution after the first call is gone.
	reconstructed, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	owner, err = reconstructed.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseCommandFixture(owner)
	progress, err = owner.Execute(ctx, req)
	if err != nil || !progress.Complete() || len(progress.Receipts()) != 2 {
		t.Fatal("exact prefix did not resume", err)
	}
	secondRecords, err := owner.ReadLedger(ctx, "second")
	if err != nil || len(secondRecords) != 1 || secondRecords[0].Version() != 1 || secondRecords[0].Checksum() != second.Checksum() {
		t.Fatal("namespace identity rewritten", err)
	}
	replay, err := owner.Execute(ctx, req)
	if err != nil || !replay.Complete() || len(replay.Receipts()) != 2 {
		t.Fatal("completed command replayed SQL", err)
	}
	req.Fingerprint[0]++
	if _, err := owner.Execute(ctx, req); !errors.Is(err, postgres.ErrCommandConflict) {
		t.Fatal("changed command fingerprint accepted", err)
	}
	req.ID = "migration_command_0003"
	req.Histories[0].Base = firstRecords
	req.Histories[1].Base = secondRecords
	req.Batches = nil
	if _, err := owner.Execute(ctx, req); !errors.Is(err, postgres.ErrCommandConflict) {
		t.Fatal("same idempotency key rebound to another command", err)
	}
}

func releaseCommandFixture(owner *postgres.CommandSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = owner.Release(ctx)
}

func TestCommandFreshPreflightIsOneAtomicUnit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	var database string
	if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	role := database + "_command"
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if _, err := db.ExecContext(cleanup, `DROP ROLE IF EXISTS "`+role+`"`); err != nil {
			t.Error("fixture role cleanup", err)
		}
	})
	first, err := migrations.NewMigration(1, "fresh_command_first", migrations.TransactionModeDefault, "CREATE TABLE public.fresh_command(id int)", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := migrations.NewMigration(2, "fresh_command_second", migrations.TransactionModeDefault, "SELECT 1/0", "")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := backend.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseCommandFixture(owner) })
	req := postgres.CommandRequest{ID: "fresh_command_identity_1", Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{3}, Histories: []postgres.CommandHistory{{Namespace: "fresh", Source: []migrations.Migration{first, second}}}, Batches: []postgres.CommandBatch{{Namespace: "fresh", Migrations: []migrations.Migration{first, second}, Preflight: `CREATE ROLE "` + role + `" NOLOGIN`}}}
	if _, err := owner.Execute(ctx, req); !errors.Is(err, migrations.ErrExecutionFailed) {
		t.Fatal("failed fresh unit accepted", err)
	}
	progress, exists, err := owner.ReadProgress(ctx, req.ID)
	if err != nil || !exists || progress.Complete() || len(progress.Receipts()) != 0 {
		t.Fatal("fresh rollback retained a prefix", err)
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.fresh_command') IS NULL AND to_regclass('public.golib_migrations_fresh') IS NULL AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&untouched); err != nil || !untouched {
		t.Fatal("fresh rollback retained preflight/schema/ledger", err)
	}
	second, err = migrations.NewMigration(2, "fresh_command_second", migrations.TransactionModeDefault, "CREATE INDEX fresh_command_id ON public.fresh_command(id)", "")
	if err != nil {
		t.Fatal(err)
	}
	req.ID = "fresh_command_identity_2"
	req.IdempotencyDigest[0]++
	req.Histories[0].Source = []migrations.Migration{first, second}
	req.Batches[0].Migrations = []migrations.Migration{first, second}
	owner, err = backend.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseCommandFixture(owner)
	progress, err = owner.Execute(ctx, req)
	if err != nil || !progress.Complete() || len(progress.Receipts()) != 2 {
		t.Fatal("complete fresh unit did not commit", err)
	}
	objects := []postgres.CatalogIdentity{
		{Kind: postgres.CatalogRole, Name: role},
		{Kind: postgres.CatalogRelation, Schema: "public", Name: "fresh_command_id"},
		{Kind: postgres.CatalogColumn, Schema: "public", Name: "fresh_command", Member: "id"},
		{Kind: postgres.CatalogFunction, Schema: "pg_catalog", Name: "lower"},
		{Kind: postgres.CatalogColumn, Schema: "public", Name: "fresh_command", Member: "missing"},
		{Kind: postgres.CatalogRole, Name: role},
	}
	presence, err := owner.CatalogPresence(ctx, objects)
	if err != nil || len(presence) != len(objects) {
		t.Fatal("catalog discovery failed", err)
	}
	for i, present := range presence {
		if present != (i != 4) {
			t.Fatal("catalog discovery lost exact identity/position", i)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if partial, err := owner.CatalogPresence(canceled, objects); !errors.Is(err, context.Canceled) || partial != nil {
		t.Fatal("canceled catalog returned evidence", err)
	}
}

func TestCommandRejectsLateDriftBeforeAnyWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, _ := isolatedMigrationDatabase(t, ctx)
	backend, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	step, err := migrations.NewMigration(1, "command_late", migrations.TransactionModeDefault, "CREATE TABLE public.command_late(id int)", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE VIEW public.golib_migrations_late AS SELECT 1 AS foreign_history"); err != nil {
		t.Fatal(err)
	}
	owner, err := backend.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = owner.Release(cleanup)
	})
	req := postgres.CommandRequest{ID: "migration_command_0002", Fingerprint: [32]byte{2}, IdempotencyDigest: [32]byte{12}, Histories: []postgres.CommandHistory{{Namespace: "early", Source: []migrations.Migration{step}}, {Namespace: "late", Source: []migrations.Migration{step}}}, Batches: []postgres.CommandBatch{{Namespace: "early", Migrations: []migrations.Migration{step}}, {Namespace: "late", Migrations: []migrations.Migration{step}}}}
	if _, err := owner.Execute(ctx, req); !errors.Is(err, postgres.ErrLedgerConflict) {
		t.Fatal("foreign final namespace accepted", err)
	}
	if err := owner.Release(ctx); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.golib_migration_commands') IS NULL AND to_regclass('public.golib_migrations_early') IS NULL AND to_regclass('public.command_late') IS NULL").Scan(&untouched); err != nil || !untouched {
		t.Fatal("late drift allowed early writes", err)
	}
}
