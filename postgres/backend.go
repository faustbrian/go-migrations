// Package postgres implements the owned PostgreSQL ledger and lock backend.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	migrations "github.com/faustbrian/golib/pkg/migrations"
	gooseadapter "github.com/faustbrian/golib/pkg/migrations/internal/goose"
)

const (
	advisoryLockKey            int64 = 0x676f6d6967726174
	defaultLockRetryInterval         = 100 * time.Millisecond
	statementTimeoutResetLimit       = 30 * time.Second
)

var (
	// ErrInvalidConfig indicates unusable PostgreSQL backend configuration.
	ErrInvalidConfig = errors.New("invalid PostgreSQL migration configuration")
	// ErrLockNotHeld indicates that advisory ownership was lost or duplicated.
	ErrLockNotHeld = errors.New("PostgreSQL migration advisory lock not held")
	// ErrSessionReleased indicates use after lock-session release.
	ErrSessionReleased = errors.New("PostgreSQL migration session released")
	// ErrLedgerConflict indicates that owned-ledger state changed unexpectedly.
	ErrLedgerConflict = errors.New("PostgreSQL migration ledger conflict")
)

const createLedgerSQL = `CREATE TABLE IF NOT EXISTS public.go_schema_migrations (
    version bigint PRIMARY KEY CHECK (version > 0),
    kind text NOT NULL CHECK (kind IN ('migration', 'baseline')),
    name text NOT NULL CHECK (name <> ''),
    checksum text NOT NULL CHECK (checksum ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    finished_at timestamptz NULL,
    execution_time_ms bigint NOT NULL DEFAULT 0 CHECK (execution_time_ms >= 0),
    dirty boolean NOT NULL,
    engine text NOT NULL,
    engine_version text NOT NULL,
    CHECK ((dirty AND finished_at IS NULL) OR (NOT dirty AND finished_at IS NOT NULL))
)`

// Option configures a PostgreSQL backend.
type Option func(*Backend) error

// WithLockRetryInterval controls polling when another job owns the advisory
// lock. Cancellation is always honored while waiting.
func WithLockRetryInterval(interval time.Duration) Option {
	return func(backend *Backend) error {
		if interval <= 0 {
			return ErrInvalidConfig
		}
		backend.lockRetryInterval = interval

		return nil
	}
}

// WithLockTimeout bounds advisory-lock polling independently of the caller's
// broader job deadline. Lock attempts repeat only while another session owns
// the lock; database errors are returned without retry.
func WithLockTimeout(timeout time.Duration) Option {
	return func(backend *Backend) error {
		if timeout <= 0 {
			return ErrInvalidConfig
		}
		backend.lockTimeout = timeout

		return nil
	}
}

// WithStatementTimeout applies PostgreSQL statement_timeout to each migration
// transaction or explicit no-transaction execution session.
func WithStatementTimeout(timeout time.Duration) Option {
	return func(backend *Backend) error {
		if timeout < time.Millisecond {
			return ErrInvalidConfig
		}
		backend.statementTimeout = timeout

		return nil
	}
}

// Backend owns PostgreSQL preparation and creates connection-bound sessions.
type Backend struct {
	database          *sql.DB
	lockRetryInterval time.Duration
	lockTimeout       time.Duration
	statementTimeout  time.Duration
}

// New constructs the PostgreSQL backend without taking ownership of database.
func New(database *sql.DB, options ...Option) (*Backend, error) {
	if database == nil {
		return nil, ErrInvalidConfig
	}

	backend := &Backend{
		database:          database,
		lockRetryInterval: defaultLockRetryInterval,
	}
	for _, option := range options {
		if option == nil {
			return nil, ErrInvalidConfig
		}
		if err := option(backend); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
	}

	return backend, nil
}

// Prepare creates only the package-owned ledger on the advisory-lock
// connection. It never reads or mutates Laravel's migrations table.
func (session *session) Prepare(ctx context.Context) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.released || session.connection == nil {
		return ErrSessionReleased
	}
	if _, err := session.connection.ExecContext(ctx, createLedgerSQL); err != nil {
		return fmt.Errorf("create go_schema_migrations: %w", err)
	}

	return nil
}

// Acquire waits for the stable package advisory lock on one physical
// connection and returns all subsequent operations bound to that connection.
func (backend *Backend) Acquire(ctx context.Context) (migrations.Session, error) {
	if backend == nil || backend.database == nil || backend.lockRetryInterval <= 0 {
		return nil, ErrInvalidConfig
	}

	acquireCtx := ctx
	cancel := func() {}
	if backend.lockTimeout > 0 {
		acquireCtx, cancel = context.WithTimeout(ctx, backend.lockTimeout)
	}
	defer cancel()

	connection, err := backend.database.Conn(acquireCtx)
	if err != nil {
		return nil, fmt.Errorf("acquire dedicated PostgreSQL connection: %w", err)
	}

	for {
		var acquired bool
		if err := connection.QueryRowContext(
			acquireCtx,
			"SELECT pg_try_advisory_lock($1)",
			advisoryLockKey,
		).Scan(&acquired); err != nil {
			_ = connection.Close()
			if contextErr := acquireCtx.Err(); contextErr != nil {
				return nil, contextErr
			}

			return nil, fmt.Errorf("try PostgreSQL advisory lock: %w", err)
		}
		if acquired {
			return &session{
				connection:       connection,
				statementTimeout: backend.statementTimeout,
			}, nil
		}

		timer := time.NewTimer(backend.lockRetryInterval)
		select {
		case <-acquireCtx.Done():
			timer.Stop()
			_ = connection.Close()

			return nil, acquireCtx.Err()
		case <-timer.C:
		}
	}
}

type session struct {
	mu               sync.Mutex
	connection       *sql.Conn
	statementTimeout time.Duration
	released         bool
}

func (session *session) Records(ctx context.Context) ([]migrations.Record, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.released || session.connection == nil {
		return nil, ErrSessionReleased
	}

	rows, err := session.connection.QueryContext(ctx, "SELECT kind, version, name, checksum, started_at, finished_at, execution_time_ms, dirty FROM public.go_schema_migrations ORDER BY version ASC")
	if err != nil {
		return nil, fmt.Errorf("query go_schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]migrations.Record, 0)
	for rows.Next() {
		var (
			kindText   string
			version    int64
			name       string
			encoded    string
			startedAt  time.Time
			finishedAt sql.NullTime
			durationMS int64
			dirty      bool
		)
		if err := rows.Scan(
			&kindText,
			&version,
			&name,
			&encoded,
			&startedAt,
			&finishedAt,
			&durationMS,
			&dirty,
		); err != nil {
			return nil, fmt.Errorf("%w: scan ledger row: %w", migrations.ErrInvalidRecord, err)
		}
		if dirty == finishedAt.Valid {
			return nil, migrations.ErrInvalidRecord
		}
		appliedAt := startedAt
		if finishedAt.Valid {
			appliedAt = finishedAt.Time
		}

		record, err := decodeRecord(kindText, version, name, encoded, appliedAt, durationMS, dirty)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query go_schema_migrations rows: %w", err)
	}

	return records, nil
}

func (session *session) Apply(ctx context.Context, migration migrations.Migration) (migrations.Record, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}

	adapter, err := gooseadapter.Compile(migration)
	if err != nil {
		return migrations.Record{}, err
	}

	startedAt := time.Now().UTC()
	if migration.TransactionMode() == migrations.TransactionModeDefault {
		return session.applyTransaction(ctx, adapter, migration, startedAt)
	}

	return session.applyWithoutTransaction(ctx, adapter, migration, startedAt)
}

func (session *session) Rollback(ctx context.Context, migration migrations.Migration) (migrations.Record, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}
	adapter, err := gooseadapter.Compile(migration)
	if err != nil {
		return migrations.Record{}, err
	}
	if migration.DownSQL() == "" {
		return migrations.Record{}, migrations.ErrIrreversible
	}
	if migration.TransactionMode() == migrations.TransactionModeDefault {
		return session.rollbackTransaction(ctx, adapter, migration)
	}

	return session.rollbackWithoutTransaction(ctx, adapter, migration)
}

// Recover persists an explicit operator-reviewed dirty outcome atomically.
func (session *session) Recover(
	ctx context.Context,
	migration migrations.Migration,
	action migrations.RecoveryAction,
) (migrations.Record, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}

	switch action {
	case migrations.RecoveryMarkApplied:
		finishedAt := time.Now().UTC()
		var startedAt time.Time
		err := session.connection.QueryRowContext(
			ctx,
			`UPDATE public.go_schema_migrations SET finished_at = $1, execution_time_ms = GREATEST(0, floor(EXTRACT(EPOCH FROM ($1 - started_at)) * 1000)::bigint), dirty = false WHERE version = $2 AND checksum = $3 AND dirty = true RETURNING started_at`,
			finishedAt,
			int64(migration.Version()),
			migration.Checksum().String(),
		).Scan(&startedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return migrations.Record{}, migrations.ErrNoDirtyMigration
		}
		if err != nil {
			return migrations.Record{}, fmt.Errorf("mark dirty migration applied: %w", err)
		}
		duration := finishedAt.Sub(startedAt).Truncate(time.Millisecond)
		if duration < 0 {
			duration = 0
		}

		return migrations.NewRecord(
			migrations.RecordKindMigration,
			migration.Version(),
			migration.Name(),
			migration.Checksum(),
			finishedAt,
			duration,
			false,
		)
	case migrations.RecoveryMarkRolledBack:
		var startedAt time.Time
		var durationMS int64
		err := session.connection.QueryRowContext(
			ctx,
			`DELETE FROM public.go_schema_migrations WHERE version = $1 AND checksum = $2 AND dirty = true RETURNING started_at, execution_time_ms`,
			int64(migration.Version()),
			migration.Checksum().String(),
		).Scan(&startedAt, &durationMS)
		if errors.Is(err, sql.ErrNoRows) {
			return migrations.Record{}, migrations.ErrNoDirtyMigration
		}
		if err != nil {
			return migrations.Record{}, fmt.Errorf("remove rolled-back dirty migration: %w", err)
		}

		return migrations.NewRecord(
			migrations.RecordKindMigration,
			migration.Version(),
			migration.Name(),
			migration.Checksum(),
			startedAt,
			time.Duration(durationMS)*time.Millisecond,
			true,
		)
	default:
		return migrations.Record{}, migrations.ErrInvalidRecovery
	}
}

func (session *session) rollbackTransaction(
	ctx context.Context,
	adapter *gooseadapter.Adapter,
	migration migrations.Migration,
) (migrations.Record, error) {
	transaction, err := session.connection.BeginTx(ctx, nil)
	if err != nil {
		return migrations.Record{}, fmt.Errorf("begin rollback transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	if err := setLocalStatementTimeout(ctx, transaction, session.statementTimeout); err != nil {
		return migrations.Record{}, err
	}

	if err := adapter.RollbackTx(ctx, transaction); err != nil {
		return migrations.Record{}, err
	}
	record, err := deleteRecord(ctx, transaction, migration)
	if err != nil {
		return migrations.Record{}, err
	}
	if err := transaction.Commit(); err != nil {
		return migrations.Record{}, fmt.Errorf("commit rollback transaction: %w", err)
	}

	return record, nil
}

func setLocalStatementTimeout(
	ctx context.Context,
	transaction *sql.Tx,
	timeout time.Duration,
) error {
	if timeout == 0 {
		return nil
	}
	_, err := transaction.ExecContext(
		ctx,
		"SELECT set_config('statement_timeout', $1, true)",
		strconv.FormatInt(timeout.Milliseconds(), 10)+"ms",
	)
	if err != nil {
		return fmt.Errorf("set local migration statement timeout: %w", err)
	}

	return nil
}

func (session *session) rollbackWithoutTransaction(
	ctx context.Context,
	adapter *gooseadapter.Adapter,
	migration migrations.Migration,
) (record migrations.Record, err error) {
	if err = session.setSessionStatementTimeout(ctx); err != nil {
		return migrations.Record{}, err
	}
	defer func() {
		err = errors.Join(err, session.resetSessionStatementTimeout(ctx))
	}()

	var appliedAt time.Time
	var durationMS int64
	err = session.connection.QueryRowContext(
		ctx,
		`UPDATE public.go_schema_migrations SET finished_at = NULL, dirty = true WHERE version = $1 AND checksum = $2 AND dirty = false RETURNING started_at, execution_time_ms`,
		int64(migration.Version()),
		migration.Checksum().String(),
	).Scan(&appliedAt, &durationMS)
	if errors.Is(err, sql.ErrNoRows) {
		return migrations.Record{}, ErrLedgerConflict
	}
	if err != nil {
		return migrations.Record{}, fmt.Errorf("mark rollback dirty: %w", err)
	}
	if err := adapter.RollbackConn(ctx, session.connection); err != nil {
		return migrations.Record{}, err
	}
	result, err := session.connection.ExecContext(
		ctx,
		`DELETE FROM public.go_schema_migrations WHERE version = $1 AND checksum = $2 AND dirty = true`,
		int64(migration.Version()),
		migration.Checksum().String(),
	)
	if err != nil {
		return migrations.Record{}, fmt.Errorf("delete rolled-back migration record: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return migrations.Record{}, fmt.Errorf("delete rolled-back migration result: %w", err)
	}
	if rows != 1 {
		return migrations.Record{}, ErrLedgerConflict
	}

	return migrations.NewRecord(
		migrations.RecordKindMigration,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		appliedAt,
		time.Duration(durationMS)*time.Millisecond,
		false,
	)
}

func (session *session) applyTransaction(
	ctx context.Context,
	adapter *gooseadapter.Adapter,
	migration migrations.Migration,
	startedAt time.Time,
) (migrations.Record, error) {
	transaction, err := session.connection.BeginTx(ctx, nil)
	if err != nil {
		return migrations.Record{}, fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	if err := setLocalStatementTimeout(ctx, transaction, session.statementTimeout); err != nil {
		return migrations.Record{}, err
	}

	if err := insertDirty(ctx, transaction, migration, startedAt); err != nil {
		return migrations.Record{}, err
	}
	if err := adapter.ApplyTx(ctx, transaction); err != nil {
		return migrations.Record{}, err
	}

	finishedAt := time.Now().UTC()
	duration := finishedAt.Sub(startedAt)
	if err := markClean(ctx, transaction, migration, finishedAt, duration); err != nil {
		return migrations.Record{}, err
	}
	if err := transaction.Commit(); err != nil {
		return migrations.Record{}, fmt.Errorf("commit migration transaction: %w", err)
	}

	return migrations.NewRecord(
		migrations.RecordKindMigration,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		finishedAt,
		duration,
		false,
	)
}

func (session *session) applyWithoutTransaction(
	ctx context.Context,
	adapter *gooseadapter.Adapter,
	migration migrations.Migration,
	startedAt time.Time,
) (record migrations.Record, err error) {
	if err = session.setSessionStatementTimeout(ctx); err != nil {
		return migrations.Record{}, err
	}
	defer func() {
		err = errors.Join(err, session.resetSessionStatementTimeout(ctx))
	}()

	if err := insertDirty(ctx, session.connection, migration, startedAt); err != nil {
		return migrations.Record{}, err
	}
	if err := adapter.ApplyConn(ctx, session.connection); err != nil {
		return migrations.Record{}, err
	}

	finishedAt := time.Now().UTC()
	duration := finishedAt.Sub(startedAt)
	if err := markClean(ctx, session.connection, migration, finishedAt, duration); err != nil {
		return migrations.Record{}, err
	}

	return migrations.NewRecord(
		migrations.RecordKindMigration,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		finishedAt,
		duration,
		false,
	)
}

func (session *session) setSessionStatementTimeout(ctx context.Context) error {
	if session.statementTimeout == 0 {
		return nil
	}
	_, err := session.connection.ExecContext(
		ctx,
		"SELECT set_config('statement_timeout', $1, false)",
		strconv.FormatInt(session.statementTimeout.Milliseconds(), 10)+"ms",
	)
	if err != nil {
		return fmt.Errorf("set migration session statement timeout: %w", err)
	}

	return nil
}

func (session *session) resetSessionStatementTimeout(ctx context.Context) error {
	if session.statementTimeout == 0 {
		return nil
	}
	resetCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		statementTimeoutResetLimit,
	)
	defer cancel()
	if _, err := session.connection.ExecContext(
		resetCtx,
		"SELECT set_config('statement_timeout', '0', false)",
	); err != nil {
		return fmt.Errorf("reset migration session statement timeout: %w", err)
	}

	return nil
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertDirty(
	ctx context.Context,
	execer contextExecer,
	migration migrations.Migration,
	startedAt time.Time,
) error {
	_, err := execer.ExecContext(
		ctx,
		`INSERT INTO public.go_schema_migrations (version, kind, name, checksum, started_at, finished_at, execution_time_ms, dirty, engine, engine_version) VALUES ($1, $2, $3, $4, $5, NULL, 0, true, 'postgres', 'v1')`,
		int64(migration.Version()),
		"migration",
		migration.Name(),
		migration.Checksum().String(),
		startedAt,
	)
	if err != nil {
		return fmt.Errorf("insert dirty migration record: %w", err)
	}

	return nil
}

func markClean(
	ctx context.Context,
	execer contextExecer,
	migration migrations.Migration,
	finishedAt time.Time,
	duration time.Duration,
) error {
	result, err := execer.ExecContext(
		ctx,
		`UPDATE public.go_schema_migrations SET finished_at = $1, execution_time_ms = $2, dirty = false WHERE version = $3 AND checksum = $4 AND dirty = true`,
		finishedAt,
		duration.Milliseconds(),
		int64(migration.Version()),
		migration.Checksum().String(),
	)
	if err != nil {
		return fmt.Errorf("complete migration record: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete migration record result: %w", err)
	}
	if rows != 1 {
		return ErrLedgerConflict
	}

	return nil
}

func deleteRecord(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	migration migrations.Migration,
) (migrations.Record, error) {
	var appliedAt time.Time
	var durationMS int64
	err := queryer.QueryRowContext(
		ctx,
		`DELETE FROM public.go_schema_migrations WHERE version = $1 AND checksum = $2 AND dirty = false RETURNING finished_at, execution_time_ms`,
		int64(migration.Version()),
		migration.Checksum().String(),
	).Scan(&appliedAt, &durationMS)
	if errors.Is(err, sql.ErrNoRows) {
		return migrations.Record{}, ErrLedgerConflict
	}
	if err != nil {
		return migrations.Record{}, fmt.Errorf("delete rolled-back migration record: %w", err)
	}

	return migrations.NewRecord(
		migrations.RecordKindMigration,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		appliedAt,
		time.Duration(durationMS)*time.Millisecond,
		false,
	)
}

func decodeRecord(
	kindText string,
	version int64,
	name string,
	encoded string,
	finishedAt time.Time,
	durationMS int64,
	dirty bool,
) (migrations.Record, error) {
	var kind migrations.RecordKind
	switch kindText {
	case "migration":
		kind = migrations.RecordKindMigration
	case "baseline":
		kind = migrations.RecordKindBaseline
	default:
		return migrations.Record{}, migrations.ErrInvalidRecord
	}
	if version <= 0 || durationMS < 0 || durationMS > math.MaxInt64/int64(time.Millisecond) {
		return migrations.Record{}, migrations.ErrInvalidRecord
	}
	checksum, err := migrations.ParseChecksum(encoded)
	if err != nil {
		return migrations.Record{}, fmt.Errorf("%w: %w", migrations.ErrInvalidRecord, err)
	}
	record, err := migrations.NewRecord(
		kind,
		migrations.Version(version),
		name,
		checksum,
		finishedAt,
		time.Duration(durationMS)*time.Millisecond,
		dirty,
	)
	if err != nil {
		return migrations.Record{}, fmt.Errorf("%w: %w", migrations.ErrInvalidRecord, err)
	}

	return record, nil
}

func (session *session) Release(ctx context.Context) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.released || session.connection == nil {
		return ErrSessionReleased
	}

	var unlocked bool
	queryErr := session.connection.QueryRowContext(
		ctx,
		"SELECT pg_advisory_unlock($1)",
		advisoryLockKey,
	).Scan(&unlocked)
	closeErr := session.connection.Close()
	session.released = true
	session.connection = nil
	if queryErr != nil {
		return errors.Join(fmt.Errorf("release PostgreSQL advisory lock: %w", queryErr), closeErr)
	}
	if !unlocked {
		return errors.Join(ErrLockNotHeld, closeErr)
	}

	return closeErr
}
