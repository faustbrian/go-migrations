//go:build integration

// Package interop_test proves that v4 and v5 runners share one PostgreSQL
// database: v5 must read, plan, append, recover and resume history that the
// published v4.0.0 wrote, and both majors must contend for the same lock.
package interop_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	v4 "github.com/faustbrian/go-migrations/v4"
	v4postgres "github.com/faustbrian/go-migrations/v4/postgres"
	v5 "github.com/faustbrian/go-migrations/v5"
	v5postgres "github.com/faustbrian/go-migrations/v5/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type step struct {
	version  uint64
	name     string
	mode     int
	up, down string
}

var (
	baselineStep = step{version: 1, name: "baseline"}
	cleanStep    = step{2, "clean_table", 0, "CREATE TABLE public.interop_clean(id int PRIMARY KEY);\nINSERT INTO public.interop_clean VALUES (1);\n", "DROP TABLE public.interop_clean;"}
	dirtyStep    = step{3, "dirty_index", 1, "SELECT 1/0;", ""}
	appendStep   = step{4, "appended_by_v5", 0, "CREATE TABLE public.interop_v5(id int);", ""}
	ownerFirst   = step{1, "owner_first", 0, "CREATE TABLE public.interop_owner(id int);", ""}
	ownerSecond  = step{2, "owner_second", 0, "ALTER TABLE public.interop_owner ADD COLUMN note text;", ""}
	commandFirst = step{1, "command_first", 0, "CREATE TABLE public.interop_command_first(id int);", ""}
	commandLater = step{2, "command_later", 0, "CREATE TABLE public.interop_command_later AS SELECT id FROM public.interop_dependency;", ""}
)

func (s step) v4(t *testing.T) v4.Migration {
	t.Helper()
	migration, err := v4.NewMigration(v4.Version(s.version), s.name, v4.TransactionMode(s.mode), s.up, s.down)
	if err != nil {
		t.Fatal(err)
	}
	return migration
}

func (s step) v5(t *testing.T) v5.Migration {
	t.Helper()
	migration, err := v5.NewMigration(v5.Version(s.version), s.name, v5.TransactionMode(s.mode), s.up, s.down)
	if err != nil {
		t.Fatal(err)
	}
	return migration
}

func TestV5ContinuesHistoryWrittenByV4(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db, pool := fixture(t, ctx)

	for _, s := range []step{cleanStep, dirtyStep, appendStep, ownerFirst, ownerSecond, commandFirst, commandLater} {
		if s.v4(t).Checksum().String() != s.v5(t).Checksum().String() {
			t.Fatalf("%s checksum differs between v4 and v5", s.name)
		}
	}

	writeV4History(t, ctx, db)

	backend, err := v5postgres.New(pool, v5postgres.WithLockTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := v4postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}

	// Read: v5 decodes exactly the rows v4 persisted.
	before, err := legacy.ReadLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	records, err := backend.ReadLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRecords(t, before, records)
	if len(records) != 3 || records[0].Kind() != v5.RecordKindBaseline || records[1].Dirty() || !records[2].Dirty() {
		t.Fatalf("v4 history = %v; want baseline, clean and dirty rows", records)
	}

	// Plan: the dirty v4 row blocks planning until v5 recovers it.
	source := []v5.Migration{cleanStep.v5(t), dirtyStep.v5(t), appendStep.v5(t)}
	if _, err := v5.PlanUp(source, records); !errors.Is(err, v5.ErrDirty) {
		t.Fatalf("PlanUp(dirty v4 history) error = %v, want ErrDirty", err)
	}
	session, err := backend.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recoverer := session.(interface {
		Recover(context.Context, v5.Migration, v5.RecoveryAction) (v5.Record, error)
	})
	if _, err := recoverer.Recover(ctx, dirtyStep.v5(t), v5.RecoveryMarkApplied); err != nil {
		t.Fatalf("v5 recovery of v4 dirty row: %v", err)
	}
	recovered, err := session.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := v5.PlanUp(source, recovered)
	if err != nil || len(plan.Steps()) != 1 || plan.Steps()[0].Migration().Version() != 4 {
		t.Fatalf("PlanUp(recovered v4 history) = %v, %v; want only version 4", plan.Steps(), err)
	}
	// Append: v5 adds a row after v4 history without rewriting it.
	if _, err := session.Apply(ctx, appendStep.v5(t)); err != nil {
		t.Fatalf("v5 append after v4 history: %v", err)
	}
	if err := session.Release(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := legacy.ReadLedger(ctx)
	if err != nil || len(after) != 4 || after[1].AppliedAt() != before[1].AppliedAt() || after[0].Checksum() != before[0].Checksum() {
		t.Fatalf("v4 reading v5-appended history = %v, %v", after, err)
	}

	// Namespaced ledger: v5 reads and appends to the v4 owner history.
	owner, err := backend.ForNamespace("interop")
	if err != nil {
		t.Fatal(err)
	}
	ownerRecords, err := owner.ReadLedger(ctx)
	if err != nil || len(ownerRecords) != 1 || ownerRecords[0].Checksum().String() != ownerFirst.v5(t).Checksum().String() {
		t.Fatalf("v5 namespaced read of v4 ledger = %v, %v", ownerRecords, err)
	}
	ownerSession, err := owner.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerSession.Apply(ctx, ownerSecond.v5(t)); err != nil {
		t.Fatalf("v5 namespaced append: %v", err)
	}
	if err := ownerSession.Release(ctx); err != nil {
		t.Fatal(err)
	}

	// Command: v5 resumes the exact v4 command from its durable prefix.
	if _, err := db.ExecContext(ctx, "CREATE TABLE public.interop_dependency AS SELECT 7 AS id"); err != nil {
		t.Fatal(err)
	}
	commands, err := backend.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = commands.Release(context.Background()) }()
	stored, exists, err := commands.ReadProgress(ctx, commandID)
	if err != nil || !exists || stored.Complete() || len(stored.Receipts()) != 1 {
		t.Fatalf("v5 read of v4 command progress = %v exists=%t: %v", stored.Receipts(), exists, err)
	}
	progress, err := commands.Execute(ctx, v5Command(t))
	if err != nil || !progress.Complete() || len(progress.Receipts()) != 2 {
		t.Fatalf("v5 resume of v4 command = %v: %v", progress.Receipts(), err)
	}
	if progress.Receipts()[0].Record.AppliedAt() != stored.Receipts()[0].Record.AppliedAt() {
		t.Fatal("v5 resume rewrote the v4 command receipt")
	}
}

func TestV4AndV5ContendForOneLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, pool := fixture(t, ctx)
	legacy, err := v4postgres.New(db, v4postgres.WithLockRetryInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	current, err := v5postgres.New(pool, v5postgres.WithLockRetryInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	held, err := legacy.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = current.Acquire(short)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("v5 acquired while v4 held the lock: %v", err)
	}
	if err := held.Release(ctx); err != nil {
		t.Fatal(err)
	}

	owned, err := current.Acquire(ctx)
	if err != nil {
		t.Fatalf("v5 could not acquire after v4 released: %v", err)
	}
	short, stop = context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = legacy.Acquire(short)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("v4 acquired while v5 held the lock: %v", err)
	}
	if err := owned.Release(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := legacy.Acquire(ctx)
	if err != nil {
		t.Fatalf("v4 could not acquire after v5 released: %v", err)
	}
	if err := again.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

const commandID = "interop_command_0001"

func v4Command(t *testing.T) v4postgres.CommandRequest {
	t.Helper()
	first, later := commandFirst.v4(t), commandLater.v4(t)
	return v4postgres.CommandRequest{
		ID: commandID, Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{2},
		Histories: []v4postgres.CommandHistory{{Namespace: "command", Source: []v4.Migration{first, later}}},
		Batches: []v4postgres.CommandBatch{
			{Namespace: "command", Migrations: []v4.Migration{first}},
			{Namespace: "command", Migrations: []v4.Migration{later}},
		},
	}
}

func v5Command(t *testing.T) v5postgres.CommandRequest {
	t.Helper()
	first, later := commandFirst.v5(t), commandLater.v5(t)
	return v5postgres.CommandRequest{
		ID: commandID, Fingerprint: [32]byte{1}, IdempotencyDigest: [32]byte{2},
		Histories: []v5postgres.CommandHistory{{Namespace: "command", Source: []v5.Migration{first, later}}},
		Batches: []v5postgres.CommandBatch{
			{Namespace: "command", Migrations: []v5.Migration{first}},
			{Namespace: "command", Migrations: []v5.Migration{later}},
		},
	}
}

// writeV4History records a baseline, a clean and a dirty row, a namespaced
// ledger and a command whose second batch fails on a missing dependency, all
// through the published v4.0.0 backend.
func writeV4History(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	backend, err := v4postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := backend.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := v4.NewBaseline(v4.Version(baselineStep.version), baselineStep.name, fingerprint)
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
	baseliner := session.(interface {
		Baseline(context.Context, v4.Baseline) (v4.Record, error)
	})
	if _, err := baseliner.Baseline(ctx, baseline); err != nil {
		t.Fatalf("v4 baseline: %v", err)
	}
	if _, err := session.Apply(ctx, cleanStep.v4(t)); err != nil {
		t.Fatalf("v4 clean apply: %v", err)
	}
	if _, err := session.Apply(ctx, dirtyStep.v4(t)); !errors.Is(err, v4.ErrExecutionFailed) {
		t.Fatalf("v4 failing no-transaction apply error = %v, want execution failure", err)
	}
	if err := session.Release(ctx); err != nil {
		t.Fatal(err)
	}

	owner, err := backend.ForNamespace("interop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.InstallFresh(ctx, []v4.Migration{ownerFirst.v4(t)}, ""); err != nil {
		t.Fatalf("v4 namespaced install: %v", err)
	}

	commands, err := backend.AcquireCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := commands.Execute(ctx, v4Command(t))
	if err == nil || progress.Complete() || len(progress.Receipts()) != 1 {
		t.Fatalf("v4 command = %v: %v; want first batch only", progress.Receipts(), err)
	}
	if err := commands.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertSameRecords(t *testing.T, legacy []v4.Record, current []v5.Record) {
	t.Helper()
	if len(legacy) != len(current) {
		t.Fatalf("v4 read %d records, v5 read %d", len(legacy), len(current))
	}
	for index := range legacy {
		l, c := legacy[index], current[index]
		if uint64(l.Version()) != uint64(c.Version()) || l.Name() != c.Name() || l.Checksum().String() != c.Checksum().String() ||
			int(l.Kind()) != int(c.Kind()) || l.Dirty() != c.Dirty() || l.Duration() != c.Duration() || !l.AppliedAt().Equal(c.AppliedAt()) {
			t.Fatalf("record %d differs: v4 %#v, v5 %#v", index, l, c)
		}
	}
}

// fixture starts one disposable PostgreSQL and opens it through both the v4
// database/sql boundary and the v5 pgx pool.
func fixture(t *testing.T, ctx context.Context) (*sql.DB, *pgxpool.Pool) {
	t.Helper()
	version := "18"
	if requested := envVersion(); requested != "" {
		version = requested
	}
	container, err := tcpostgres.Run(ctx, "postgres:"+version+"-alpine",
		tcpostgres.WithDatabase("interop"), tcpostgres.WithUsername("interop"),
		tcpostgres.WithPassword("interop"), tcpostgres.BasicWaitStrategies())
	if container != nil {
		testcontainers.CleanupContainer(t, container)
	}
	if err != nil {
		t.Fatalf("start PostgreSQL %s: %v", version, err)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return db, pool
}
