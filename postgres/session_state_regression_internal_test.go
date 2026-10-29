package postgres

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestReleaseDiscardsUncertainLockConnection(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "healthy unlock reuses connection"
		if failed {
			name = "failed unlock discards connection"
		}
		t.Run(name, func(t *testing.T) {
			connector := &statePool{unlockFailure: failed}
			backend, err := newBackend(connector)
			if err != nil {
				t.Fatal(err)
			}
			owned, err := backend.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			original := connector.connections[0]
			err = owned.Release(context.Background())
			if failed {
				if !errors.Is(err, ErrDatabaseOperationFailed) || strings.Contains(err.Error(), "private") {
					t.Fatalf("release error = %v, want redacted database failure", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			next := connector.nextBorrower(t)
			reused, locked := next == original, next.locked
			if failed && (reused || !original.closed || locked) {
				t.Fatalf("uncertain session reused=%t physically closed=%t next borrower locked=%t", reused, original.closed, locked)
			}
			if !failed && (!reused || original.closed || locked) {
				t.Fatalf("healthy session reused=%t physically closed=%t next borrower locked=%t", reused, original.closed, locked)
			}
		})
	}
}

func TestAcquireDiscardsUncertainLockAndReusesDefiniteContention(t *testing.T) {
	for _, uncertain := range []bool{true, false} {
		name := "definite contention cancellation reuses connection"
		if uncertain {
			name = "uncertain scan failure discards connection"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			connector := &statePool{acquireScanFailure: uncertain}
			if !uncertain {
				connector.contendedCancel = cancel
			}
			backend, err := newBackend(connector, WithLockRetryInterval(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			owned, err := backend.Acquire(ctx)
			if owned != nil {
				t.Fatal("failed acquisition returned a session")
			}
			if uncertain {
				if !errors.Is(err, ErrDatabaseOperationFailed) || strings.Contains(err.Error(), "private") {
					t.Fatalf("acquisition error = %v, want redacted database failure", err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("contended acquisition error = %v, want cancellation", err)
			}
			original := connector.connections[0]
			next := connector.nextBorrower(t)
			reused, locked := next == original, next.locked
			if uncertain && (reused || !original.closed || locked) {
				t.Fatalf("uncertain acquisition reused=%t physically closed=%t next borrower locked=%t", reused, original.closed, locked)
			}
			if !uncertain && (!reused || original.closed || locked) {
				t.Fatalf("definite contention reused=%t physically closed=%t next borrower locked=%t", reused, original.closed, locked)
			}
		})
	}
}

func TestNoTransactionRollbackRejectsInvalidDurationWithoutChangingLedger(t *testing.T) {
	for _, duration := range []int64{-1, math.MaxInt64} {
		connector := &statePool{duration: duration}
		backend, err := newBackend(connector)
		if err != nil {
			t.Fatal(err)
		}
		owned, err := backend.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		migration, err := migrations.NewMigration(1, "state_probe", migrations.TransactionModeNone, "SELECT 1;", "SELECT down_probe;")
		if err != nil {
			t.Fatal(err)
		}
		_, err = owned.Rollback(context.Background(), migration)
		connection := connector.connections[0]
		if !errors.Is(err, migrations.ErrInvalidRecord) && !errors.Is(err, ErrLedgerConflict) {
			t.Fatalf("duration %d: rollback error = %v, want owned invalid-state error", duration, err)
		}
		if connection.dirty || connection.downExecuted {
			t.Fatalf("duration %d: malformed clean ledger changed dirty=%t down executed=%t", duration, connection.dirty, connection.downExecuted)
		}
		if err := owned.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFailedStatementTimeoutPolicyDiscardsPhysicalSession(t *testing.T) {
	for _, phase := range []string{"set", "reset"} {
		t.Run(phase, func(t *testing.T) {
			connector := &statePool{timeoutFailure: phase}
			backend, err := newBackend(connector)
			if err != nil {
				t.Fatal(err)
			}
			owned, err := backend.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			migration, err := migrations.NewMigration(1, "policy_probe", migrations.TransactionModeNone, "SELECT 1;", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := owned.Apply(context.Background(), migration); !errors.Is(err, ErrDatabaseOperationFailed) || strings.Contains(err.Error(), "private") {
				t.Fatalf("apply error = %v, want redacted policy failure", err)
			}
			original := connector.connections[0]
			if err := owned.Release(context.Background()); err != nil {
				t.Fatal(err)
			}
			next := connector.nextBorrower(t)
			if next == original || !original.closed || next.timeoutChanged || next.locked {
				t.Fatalf("policy failure reused=%t closed=%t next timeout changed=%t next locked=%t", next == original, original.closed, next.timeoutChanged, next.locked)
			}
		})
	}
}

func TestRecoveryMarkAppliedPreservesDirtyHistoryBeyondDurationRange(t *testing.T) {
	connector := &statePool{recoveryStartedAt: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)}
	backend, err := newBackend(connector)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	migration, err := migrations.NewMigration(1, "recovery_probe", migrations.TransactionModeNone, "SELECT 1;", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = owned.(*session).Recover(context.Background(), migration, migrations.RecoveryMarkApplied)
	if !errors.Is(err, migrations.ErrNoDirtyMigration) && !errors.Is(err, migrations.ErrInvalidRecord) {
		t.Fatalf("recovery error = %v, want owned invalid-history error", err)
	}
	if !connector.connections[0].dirty {
		t.Fatal("out-of-range recovery cleared dirty state")
	}
	if err := owned.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The pool models server-owned state rather than expected call choreography:
// ledger UPDATEs mutate state, and physical close releases session-level locks.
// Like pgxpool, release returns a session for reuse and discard removes it.
type statePool struct {
	timeoutFailure     string
	recoveryStartedAt  time.Time
	connections        []*stateConnection
	idle               []*stateConnection
	unlockFailure      bool
	acquireScanFailure bool
	contendedCancel    context.CancelFunc
	duration           int64
}

func (pool *statePool) acquire(context.Context) (connection, error) {
	if count := len(pool.idle); count > 0 {
		reused := pool.idle[count-1]
		pool.idle = pool.idle[:count-1]
		return reused, nil
	}
	connection := &stateConnection{
		pool:               pool,
		timeoutFailure:     pool.timeoutFailure,
		recoveryStartedAt:  pool.recoveryStartedAt,
		dirty:              !pool.recoveryStartedAt.IsZero(),
		unlockFailure:      pool.unlockFailure,
		acquireScanFailure: pool.acquireScanFailure,
		contendedCancel:    pool.contendedCancel,
		duration:           pool.duration,
	}
	pool.connections = append(pool.connections, connection)
	return connection, nil
}

func (*statePool) begin(context.Context, pgx.TxOptions) (transaction, error) {
	return nil, errors.New("transaction unsupported")
}

// nextBorrower leases the session the pool would hand the next caller.
func (pool *statePool) nextBorrower(t *testing.T) *stateConnection {
	t.Helper()
	next, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return next.(*stateConnection)
}

type stateConnection struct {
	pool               *statePool
	timeoutFailure     string
	timeoutChanged     bool
	recoveryStartedAt  time.Time
	unlockFailure      bool
	acquireScanFailure bool
	contendedCancel    context.CancelFunc
	duration           int64
	locked             bool
	closed             bool
	dirty              bool
	downExecuted       bool
}

func (*stateConnection) begin(context.Context, pgx.TxOptions) (transaction, error) {
	return nil, errors.New("transaction unsupported")
}

func (connection *stateConnection) release() {
	if !connection.closed {
		connection.pool.idle = append(connection.pool.idle, connection)
	}
}

func (connection *stateConnection) discard(context.Context) error {
	connection.closed = true
	connection.locked = false
	return nil
}

func (connection *stateConnection) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "set_config('statement_timeout'") {
		connection.timeoutChanged = true
		if connection.timeoutFailure == "set" {
			return pgconn.CommandTag{}, errors.New("private set policy diagnostic")
		}
	}
	if query == "RESET statement_timeout" {
		if connection.timeoutFailure == "reset" {
			return pgconn.CommandTag{}, errors.New("private reset policy diagnostic")
		}
		connection.timeoutChanged = false
	}
	if strings.Contains(query, "down_probe") {
		connection.downExecuted = true
	}
	return MockResult("SELECT", 1), nil
}

func (*stateConnection) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unsupported state query")
}

func (connection *stateConnection) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	switch {
	case strings.Contains(query, "pg_try_advisory_lock"):
		if connection.contendedCancel != nil {
			return stateRow{values: []any{false}, onClose: connection.contendedCancel}
		}
		connection.locked = true
		if connection.acquireScanFailure {
			// The server acquired the lock, but the reply cannot be decoded as
			// a boolean, so the lock state the session holds is uncertain.
			return stateRow{values: []any{"private malformed lock reply"}}
		}
		return stateRow{values: []any{true}}
	case strings.Contains(query, "pg_advisory_unlock"):
		if connection.unlockFailure {
			return stateRow{err: errors.New("private unlock diagnostic")}
		}
		connection.locked = false
		return stateRow{values: []any{true}}
	case strings.HasPrefix(query, "UPDATE public.migrations"):
		if !connection.recoveryStartedAt.IsZero() {
			finishedAt := args[0].(time.Time)
			duration := finishedAt.UnixMilli() - connection.recoveryStartedAt.UnixMilli()
			if strings.Contains(query, "floor(EXTRACT(EPOCH FROM ($1 - started_at)) * 1000)) <= $4") && duration > args[3].(int64) {
				return stateRow{}
			}
			connection.dirty = false
			if strings.Contains(query, "RETURNING execution_time_ms") {
				return stateRow{values: []any{duration}}
			}
			if strings.Contains(query, "RETURNING started_at, execution_time_ms") {
				return stateRow{values: []any{connection.recoveryStartedAt, duration}}
			}
			return stateRow{values: []any{connection.recoveryStartedAt}}
		}
		// Apply the duration predicate, if present, before the modeled server
		// writes dirty state, exactly as PostgreSQL evaluates UPDATE's WHERE.
		if strings.Contains(query, "execution_time_ms BETWEEN 0 AND $3") {
			bound := args[2].(int64)
			if connection.duration < 0 || connection.duration > bound {
				return stateRow{}
			}
		}
		connection.dirty = true
		return stateRow{values: []any{time.Unix(1_700_000_000, 0).UTC(), connection.duration}}
	default:
		return stateRow{err: errors.New("unsupported state query")}
	}
}

type stateRow struct {
	values  []any
	err     error
	onClose func()
}

func (row stateRow) Scan(dest ...any) error {
	if row.onClose != nil {
		defer row.onClose()
	}
	if row.err != nil {
		return row.err
	}
	if len(row.values) == 0 {
		return pgx.ErrNoRows
	}
	if len(dest) != len(row.values) {
		return errors.New("scan destination count differs")
	}
	for index, value := range row.values {
		if err := assignMockValue(dest[index], value); err != nil {
			return err
		}
	}
	return nil
}
