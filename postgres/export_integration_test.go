//go:build integration

package postgres

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CommandFault injects acknowledgement and cleanup failures around real pgx
// statements and transactions. Every statement still reaches PostgreSQL unless
// a fault replaces it, so catalog reads establish the actual server outcome.
type CommandFault struct {
	// Before rolls back the second commit and reports a lost acknowledgement.
	Before bool
	// Checkpoint fails the command checkpoint UPDATE and arms rollback faults.
	Checkpoint bool
	// Rollback fails armed rollbacks without rolling back on the server.
	Rollback bool
	// FailReservation fails the command reservation INSERT and arms rollback.
	FailReservation bool
	// RollbackArmed makes the next rollback fail when Rollback is set.
	RollbackArmed atomic.Bool
	// Discarded counts physical sessions closed instead of released.
	Discarded atomic.Int64
	commits   atomic.Int64
}

// NewFaultBackend constructs a backend over pool whose sessions inject fault.
func NewFaultBackend(pool *pgxpool.Pool, fault *CommandFault, options ...Option) (*Backend, error) {
	return newBackend(faultPool{pgxPool: pgxPool{pool}, fault: fault}, options...)
}

type faultPool struct {
	pgxPool
	fault *CommandFault
}

func (pool faultPool) acquire(ctx context.Context) (connection, error) {
	leased, err := pool.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	return faultConnection{pgxConnection: pgxConnection{leased}, fault: pool.fault}, nil
}

type faultConnection struct {
	pgxConnection
	fault *CommandFault
}

func (connection faultConnection) begin(ctx context.Context, options pgx.TxOptions) (transaction, error) {
	tx, err := connection.pgxConnection.begin(ctx, options)
	if err != nil {
		return nil, err
	}

	return faultTransaction{transaction: tx, fault: connection.fault}, nil
}

func (connection faultConnection) discard(ctx context.Context) error {
	connection.fault.Discarded.Add(1)

	return connection.pgxConnection.discard(ctx)
}

type faultTransaction struct {
	transaction
	fault *CommandFault
}

func (tx faultTransaction) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if tx.fault.FailReservation && strings.HasPrefix(query, "INSERT INTO public.golib_migration_commands(") {
		tx.fault.RollbackArmed.Store(true)
		return pgconn.CommandTag{}, io.ErrUnexpectedEOF
	}
	if tx.fault.Checkpoint && strings.HasPrefix(query, "UPDATE public.golib_migration_commands SET receipts=") {
		tx.fault.RollbackArmed.Store(true)
		return pgconn.CommandTag{}, io.ErrUnexpectedEOF
	}

	return tx.transaction.Exec(ctx, query, args...)
}

func (tx faultTransaction) Commit(ctx context.Context) error {
	if tx.fault.Checkpoint || tx.fault.Rollback || tx.fault.commits.Add(1) != 2 {
		return tx.transaction.Commit(ctx)
	}
	if tx.fault.Before {
		if err := tx.transaction.Rollback(ctx); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if err := tx.transaction.Commit(ctx); err != nil {
		return err
	}

	return io.ErrUnexpectedEOF
}

func (tx faultTransaction) Rollback(ctx context.Context) error {
	if tx.fault.Rollback && tx.fault.RollbackArmed.Load() {
		return io.ErrUnexpectedEOF
	}

	return tx.transaction.Rollback(ctx)
}

// LeasedBackendPID leases one session through the production pgx adapter,
// reports its server process ID and ends the lease by release or discard.
func LeasedBackendPID(ctx context.Context, pool *pgxpool.Pool, discard bool) (int32, error) {
	leased, err := pgxPool{pool}.acquire(ctx)
	if err != nil {
		return 0, err
	}
	var pid int32
	err = leased.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	if discard {
		return pid, errors.Join(err, leased.discard(ctx))
	}
	leased.release()

	return pid, err
}
