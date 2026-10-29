package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queryer is the statement surface shared by a leased connection and a
// transaction. Exec without arguments uses PostgreSQL's simple protocol, so
// multi-statement migration SQL executes as one request.
type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type transaction interface {
	queryer
	Commit(context.Context) error
	Rollback(context.Context) error
}

type transactionBeginner interface {
	begin(context.Context, pgx.TxOptions) (transaction, error)
}

// connection is one leased physical PostgreSQL session. Session-level state
// such as advisory locks and statement_timeout lives on it, so the owner must
// either release a known-clean session or discard it.
type connection interface {
	queryer
	transactionBeginner
	// release returns the session to the pool. pgxpool still destroys a
	// closed, busy or in-transaction session instead of reusing it.
	release()
	// discard closes the physical session so the pool never reuses it.
	discard(context.Context) error
}

// connectionPool is the backend's narrow view of *pgxpool.Pool. It exists so
// failure tests can inject faults at the same boundary production uses.
type connectionPool interface {
	transactionBeginner
	acquire(context.Context) (connection, error)
}

type pgxPool struct{ pool *pgxpool.Pool }

func (database pgxPool) acquire(ctx context.Context) (connection, error) {
	leased, err := database.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	return pgxConnection{leased}, nil
}

func (database pgxPool) begin(ctx context.Context, options pgx.TxOptions) (transaction, error) {
	tx, err := database.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}

	return pgxTransaction{tx}, nil
}

type pgxConnection struct{ *pgxpool.Conn }

func (leased pgxConnection) begin(ctx context.Context, options pgx.TxOptions) (transaction, error) {
	tx, err := leased.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}

	return pgxTransaction{tx}, nil
}

func (leased pgxConnection) release() { leased.Release() }

// Hijack removes the session from the pool before Close sends Terminate, so a
// session with uncertain lock or setting state cannot reach another borrower.
func (leased pgxConnection) discard(ctx context.Context) error {
	return leased.Hijack().Close(ctx)
}

type pgxTransaction struct{ pgx.Tx }

// Rollback treats a transaction whose connection already closed as rolled
// back: PostgreSQL aborts an open transaction when its session ends, so it can
// no longer commit. pgx closes the connection when caller cancellation
// interrupts a statement; reporting that as a rollback failure would turn pure
// cancellation into an apparent independent cleanup failure. Rollback still
// runs so a pool-level transaction returns its connection to the pool.
func (tx pgxTransaction) Rollback(ctx context.Context) error {
	closed := tx.Conn().IsClosed()
	err := tx.Tx.Rollback(ctx)
	if closed {
		return nil
	}

	return err
}
