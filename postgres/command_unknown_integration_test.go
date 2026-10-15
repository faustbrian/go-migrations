//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v3"
	"github.com/faustbrian/go-migrations/v3/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Faults preserve real pgx/PostgreSQL SQL and transaction execution. Selected
// acknowledgement or cleanup boundaries fail explicitly; raw catalog reads
// establish server outcomes independently of those failures.
type commandCommitConnector struct {
	base            driver.Connector
	commits         atomic.Int64
	before          bool
	checkpoint      bool
	rollback        bool
	failReservation bool
	closed          atomic.Int64
	rollbackArmed   atomic.Bool
}

func (c *commandCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &commandCommitConn{Conn: conn, owner: c}, nil
}
func (c *commandCommitConnector) Driver() driver.Driver { return c.base.Driver() }

type commandCommitConn struct {
	driver.Conn
	owner *commandCommitConnector
}

func (c *commandCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &commandCommitTx{Tx: tx, owner: c.owner}, nil
}
func (c *commandCommitConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if c.owner.failReservation && strings.HasPrefix(q, "INSERT INTO public.golib_migration_commands(") {
		c.owner.rollbackArmed.Store(true)
		return nil, io.ErrUnexpectedEOF
	}
	if c.owner.checkpoint && strings.HasPrefix(q, "UPDATE public.golib_migration_commands SET receipts=") {
		c.owner.rollbackArmed.Store(true)
		return nil, io.ErrUnexpectedEOF
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (c *commandCommitConn) Close() error {
	c.owner.closed.Add(1)
	return c.Conn.Close()
}
func (c *commandCommitConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *commandCommitConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

type commandCommitTx struct {
	driver.Tx
	owner *commandCommitConnector
}

func (t *commandCommitTx) Commit() error {
	if t.owner.checkpoint || t.owner.rollback {
		return t.Tx.Commit()
	}
	if t.owner.commits.Add(1) != 2 {
		return t.Tx.Commit()
	}
	if t.owner.before {
		if err := t.Tx.Rollback(); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (t *commandCommitTx) Rollback() error {
	if t.owner.rollback && t.owner.rollbackArmed.Load() {
		return io.ErrUnexpectedEOF
	}
	return t.Tx.Rollback()
}

func TestCommandRollbackFailureDiscardsOwnership(t *testing.T) {
	for _, name := range []string{"batch", "reservation", "discovery"} {
		t.Run(name, func(t *testing.T) {
			reservation, discovery := name == "reservation", name == "discovery"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			db, _ := isolatedMigrationDatabase(t, ctx)
			config := commandFixturePGXConfig(t, ctx, db)
			fault := &commandCommitConnector{base: stdlib.GetConnector(*config), checkpoint: !reservation && !discovery, rollback: true, failReservation: reservation}
			fault.rollbackArmed.Store(discovery)
			faultDB := sql.OpenDB(fault)
			t.Cleanup(func() { _ = faultDB.Close() })
			backend, err := postgres.New(faultDB)
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
			if errors.Is(executeErr, postgres.ErrCommandOutcomeUnknown) == discovery || !errors.Is(executeErr, postgres.ErrDatabaseOperationFailed) || !errors.Is(readErr, postgres.ErrSessionReleased) || releaseErr != nil || fault.closed.Load() != 1 {
				t.Fatal("uncertain rollback remained reusable or pooled", executeErr, readErr, releaseErr, fault.closed.Load())
			}
			var untouched bool
			if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.rollback_command') IS NULL AND to_regclass('public.golib_migrations_rollback') IS NULL").Scan(&untouched); err != nil || !untouched {
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
	config := commandFixturePGXConfig(t, ctx, db)
	faultDB := sql.OpenDB(&commandCommitConnector{base: stdlib.GetConnector(*config), checkpoint: true})
	t.Cleanup(func() { _ = faultDB.Close() })
	backend, err := postgres.New(faultDB)
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
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.checkpoint_command') IS NULL AND to_regclass('public.golib_migrations_checkpoint') IS NULL").Scan(&untouched); err != nil || !untouched {
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
			// Obtain the fixture DSN without logging it, preserving unique DB scope.
			config := commandFixturePGXConfig(t, ctx, db)
			fault := &commandCommitConnector{base: stdlib.GetConnector(*config), before: before}
			faultDB := sql.OpenDB(fault)
			t.Cleanup(func() { _ = faultDB.Close() })
			faultDB.SetMaxOpenConns(1)
			backend, err := postgres.New(faultDB)
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
			if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.unknown_command') IS NOT NULL").Scan(&effect); err != nil || effect == before {
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

// Clone the existing disposable connection's configuration so competing/fault
// clients use exactly that fixture, whether supplied locally or container-owned.
func commandFixturePGXConfig(t *testing.T, ctx context.Context, db *sql.DB) *pgx.ConnConfig {
	t.Helper()
	connection, err := db.Conn(ctx)
	if err != nil {
		t.Fatal("cannot borrow disposable fixture connection")
	}
	defer connection.Close()
	var config *pgx.ConnConfig
	if err := connection.Raw(func(raw any) error {
		conn, ok := raw.(*stdlib.Conn)
		if !ok {
			return errors.New("unexpected disposable driver")
		}
		config = conn.Conn().Config()
		return nil
	}); err != nil {
		t.Fatal("cannot clone disposable fixture configuration")
	}
	return config
}
