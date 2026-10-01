package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v2"
)

func TestReleaseDiscardsUncertainLockConnection(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "healthy unlock reuses connection"
		if failed {
			name = "failed unlock discards connection"
		}
		t.Run(name, func(t *testing.T) {
			connector := &stateConnector{unlockFailure: failed}
			database := stateDatabase(t, connector)
			backend, err := New(database)
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
			borrowed, err := database.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = borrowed.Close() }()
			var reused, locked bool
			if err := borrowed.Raw(func(raw any) error {
				connection := raw.(*stateConnection)
				reused, locked = connection == original, connection.locked
				return nil
			}); err != nil {
				t.Fatal(err)
			}
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
			connector := &stateConnector{acquireScanFailure: uncertain}
			if !uncertain {
				connector.contendedCancel = cancel
			}
			database := stateDatabase(t, connector)
			backend, err := New(database, WithLockRetryInterval(time.Hour))
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
			borrowed, err := database.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = borrowed.Close() }()
			var reused, locked bool
			if err := borrowed.Raw(func(raw any) error {
				connection := raw.(*stateConnection)
				reused, locked = connection == original, connection.locked
				return nil
			}); err != nil {
				t.Fatal(err)
			}
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
		connector := &stateConnector{duration: duration}
		database := stateDatabase(t, connector)
		backend, err := New(database)
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
			connector := &stateConnector{timeoutFailure: phase}
			database := stateDatabase(t, connector)
			backend, err := New(database)
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
			borrowed, err := database.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = borrowed.Close() }()
			if err := borrowed.Raw(func(raw any) error {
				next := raw.(*stateConnection)
				if next == original || !original.closed || next.timeoutChanged || next.locked {
					t.Fatalf("policy failure reused=%t closed=%t next timeout changed=%t next locked=%t", next == original, original.closed, next.timeoutChanged, next.locked)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryMarkAppliedPreservesDirtyHistoryBeyondDurationRange(t *testing.T) {
	connector := &stateConnector{recoveryStartedAt: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)}
	database := stateDatabase(t, connector)
	backend, err := New(database)
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

// The driver models server-owned state rather than expected call choreography:
// ledger UPDATEs mutate state, and physical Close releases session-level locks.
// database/sql itself owns connection reuse and discard in these regressions.
type stateConnector struct {
	timeoutFailure     string
	recoveryStartedAt  time.Time
	connections        []*stateConnection
	unlockFailure      bool
	acquireScanFailure bool
	contendedCancel    context.CancelFunc
	duration           int64
}

func (connector *stateConnector) Connect(context.Context) (driver.Conn, error) {
	connection := &stateConnection{
		timeoutFailure:     connector.timeoutFailure,
		recoveryStartedAt:  connector.recoveryStartedAt,
		dirty:              !connector.recoveryStartedAt.IsZero(),
		unlockFailure:      connector.unlockFailure,
		acquireScanFailure: connector.acquireScanFailure,
		contendedCancel:    connector.contendedCancel,
		duration:           connector.duration,
	}
	connector.connections = append(connector.connections, connection)
	return connection, nil
}

func (*stateConnector) Driver() driver.Driver { return stateDriver{} }

type stateDriver struct{}

func (stateDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector required")
}

type stateConnection struct {
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

func (*stateConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}

func (*stateConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("transaction unsupported")
}

func (connection *stateConnection) Close() error {
	connection.closed = true
	connection.locked = false
	return nil
}

func (connection *stateConnection) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "set_config('statement_timeout'") {
		connection.timeoutChanged = true
		if connection.timeoutFailure == "set" {
			return nil, errors.New("private set policy diagnostic")
		}
	}
	if query == "RESET statement_timeout" {
		if connection.timeoutFailure == "reset" {
			return nil, errors.New("private reset policy diagnostic")
		}
		connection.timeoutChanged = false
	}
	if strings.Contains(query, "down_probe") {
		connection.downExecuted = true
	}
	return driver.RowsAffected(1), nil
}

func (connection *stateConnection) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "pg_try_advisory_lock"):
		if connection.contendedCancel != nil {
			return &stateRows{values: []driver.Value{false}, onClose: connection.contendedCancel}, nil
		}
		connection.locked = true
		if connection.acquireScanFailure {
			// The server acquired the lock, but the reply cannot be decoded as
			// a boolean. This is not driver.ErrBadConn and sql must not discard
			// the physical connection automatically on our behalf.
			return &stateRows{values: []driver.Value{"private malformed lock reply"}}, nil
		}
		return &stateRows{values: []driver.Value{true}}, nil
	case strings.Contains(query, "pg_advisory_unlock"):
		if connection.unlockFailure {
			return nil, errors.New("private unlock diagnostic")
		}
		connection.locked = false
		return &stateRows{values: []driver.Value{true}}, nil
	case strings.HasPrefix(query, "UPDATE public.migrations"):
		if !connection.recoveryStartedAt.IsZero() {
			finishedAt := args[0].Value.(time.Time)
			duration := finishedAt.UnixMilli() - connection.recoveryStartedAt.UnixMilli()
			if strings.Contains(query, "floor(EXTRACT(EPOCH FROM ($1 - started_at)) * 1000)) <= $4") && duration > args[3].Value.(int64) {
				return &stateRows{}, nil
			}
			connection.dirty = false
			if strings.Contains(query, "RETURNING execution_time_ms") {
				return &stateRows{values: []driver.Value{duration}}, nil
			}
			if strings.Contains(query, "RETURNING started_at, execution_time_ms") {
				return &stateRows{values: []driver.Value{connection.recoveryStartedAt, duration}}, nil
			}
			return &stateRows{values: []driver.Value{connection.recoveryStartedAt}}, nil
		}
		// Apply the duration predicate, if present, before the modeled server
		// writes dirty state, exactly as PostgreSQL evaluates UPDATE's WHERE.
		if strings.Contains(query, "execution_time_ms BETWEEN 0 AND $3") {
			bound := args[2].Value.(int64)
			if connection.duration < 0 || connection.duration > bound {
				return &stateRows{}, nil
			}
		}
		connection.dirty = true
		return &stateRows{values: []driver.Value{time.Unix(1_700_000_000, 0).UTC(), connection.duration}}, nil
	default:
		return nil, errors.New("unsupported state query")
	}
}

type stateRows struct {
	values  []driver.Value
	read    bool
	onClose func()
}

func (rows *stateRows) Columns() []string {
	if len(rows.values) == 2 {
		return []string{"started_at", "execution_time_ms"}
	}
	return []string{"result"}
}

func (rows *stateRows) Close() error {
	if rows.onClose != nil {
		rows.onClose()
		rows.onClose = nil
	}
	return nil
}

func (rows *stateRows) Next(values []driver.Value) error {
	if rows.read || len(rows.values) == 0 {
		return io.EOF
	}
	rows.read = true
	copy(values, rows.values)
	return nil
}

func stateDatabase(t *testing.T, connector *stateConnector) *sql.DB {
	t.Helper()
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close state database: %v", err)
		}
	})
	return database
}
