// Package postgres implements the owned PostgreSQL ledger and lock backend.
package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	migrations "github.com/faustbrian/go-migrations/v3"
	gooseadapter "github.com/faustbrian/go-migrations/v3/internal/goose"
)

const (
	advisoryLockKey int64 = 0x676f6d6967726174
	// MaxLedgerRecords permits one baseline plus the complete bounded migration
	// source while preventing a hostile ledger from growing retained state.
	MaxLedgerRecords = migrations.MaxMigrationFiles + 1
	// MaxLedgerBytes bounds aggregate text retained from one ledger read.
	MaxLedgerBytes = 16 << 20
	// Literal nanoseconds keep these safety budgets indivisible while retaining
	// their exact documented durations.
	defaultLockRetryInterval    time.Duration = 100_000_000
	defaultLockTimeout          time.Duration = 30_000_000_000
	defaultStatementTimeout     time.Duration = 300_000_000_000
	defaultOperationTimeout     time.Duration = 600_000_000_000
	statementTimeoutResetLimit  time.Duration = 30_000_000_000
	maximumDurationMilliseconds               = math.MaxInt64 / int64(time.Millisecond)
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
	// ErrDatabaseOperationFailed identifies a PostgreSQL driver failure whose
	// diagnostic is available only through errors.Is or errors.As.
	ErrDatabaseOperationFailed = errors.New("PostgreSQL migration database operation failed")
	// ErrResourceLimit indicates that PostgreSQL returned more migration or
	// schema state than the package can safely retain.
	ErrResourceLimit = errors.New("PostgreSQL migration resource limit exceeded")
)

type databaseError struct {
	operation string
	cause     error
}

func (err *databaseError) Error() string {
	return err.operation + ": " + ErrDatabaseOperationFailed.Error()
}

func (err *databaseError) Format(state fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(state, "%q", err.Error())

		return
	}

	_, _ = fmt.Fprint(state, err.Error())
}

func (err *databaseError) Unwrap() []error {
	return []error{ErrDatabaseOperationFailed, err.cause}
}

func databaseFailure(operation string, cause error) error {
	if cause == nil {
		return nil
	}

	return &databaseError{operation: operation, cause: cause}
}

func databaseContextFailure(ctx context.Context, operation string, cause error) error {
	failure := databaseFailure(operation, cause)
	if failure == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return errors.Join(failure, contextErr)
	}

	return failure
}

// Ledger adoption runs on the advisory-lock connection before any history reads.
// Refuse ambiguous ownership rather than merge histories or replay migrations.
const createLedgerSQL = `DO $ledger$
BEGIN
    IF to_regclass('public.go_schema_migrations') IS NOT NULL THEN
        IF to_regclass('public.migrations') IS NOT NULL THEN
            RAISE EXCEPTION 'both migration ledger names exist';
        END IF;
        ALTER TABLE public.go_schema_migrations RENAME TO migrations;
    END IF;
END
$ledger$;
CREATE TABLE IF NOT EXISTS public.migrations (
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

// WithLockTimeout replaces the finite default advisory-lock polling timeout.
// The override must remain positive, so callers cannot disable the bound.
// Lock attempts repeat only while another session owns the lock; database
// errors are returned without retry.
func WithLockTimeout(timeout time.Duration) Option {
	return func(backend *Backend) error {
		if timeout <= 0 {
			return ErrInvalidConfig
		}
		backend.lockTimeout = timeout

		return nil
	}
}

// WithStatementTimeout replaces the finite default PostgreSQL statement_timeout
// applied to each migration transaction or explicit no-transaction execution
// session. The override must be at least one millisecond, so callers cannot
// silently select PostgreSQL's unbounded zero value.
func WithStatementTimeout(timeout time.Duration) Option {
	return func(backend *Backend) error {
		if timeout < time.Millisecond {
			return ErrInvalidConfig
		}
		backend.statementTimeout = timeout

		return nil
	}
}

// WithOperationTimeout replaces the finite default deadline covering each
// complete PostgreSQL session or schema-inspection operation. The override
// must be at least one millisecond, so callers cannot disable the bound.
func WithOperationTimeout(timeout time.Duration) Option {
	return func(backend *Backend) error {
		if timeout < time.Millisecond {
			return ErrInvalidConfig
		}
		backend.operationTimeout = timeout

		return nil
	}
}

// Backend owns PostgreSQL preparation and creates connection-bound sessions.
type Backend struct {
	database          *sql.DB
	lockRetryInterval time.Duration
	lockTimeout       time.Duration
	statementTimeout  time.Duration
	operationTimeout  time.Duration
}

// New constructs the PostgreSQL backend without taking ownership of database.
func New(database *sql.DB, options ...Option) (*Backend, error) {
	if database == nil {
		return nil, ErrInvalidConfig
	}

	backend := &Backend{
		database:          database,
		lockRetryInterval: defaultLockRetryInterval,
		lockTimeout:       defaultLockTimeout,
		statementTimeout:  defaultStatementTimeout,
		operationTimeout:  defaultOperationTimeout,
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

// Prepare adopts the legacy ledger name or creates public.migrations on the
// advisory-lock connection. Existing Laravel history must be relocated first.
func (session *session) Prepare(ctx context.Context) error {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if session.released || session.connection == nil {
		return ErrSessionReleased
	}
	if _, err := session.connection.ExecContext(operationCtx, createLedgerSQL); err != nil {
		return databaseContextFailure(operationCtx, "create migration ledger", err)
	}

	return nil
}

// Acquire waits for the stable package advisory lock on one physical
// connection and returns all subsequent operations bound to that connection.
func (backend *Backend) Acquire(ctx context.Context) (migrations.Session, error) {
	if backend == nil || backend.database == nil {
		return nil, ErrInvalidConfig
	}

	acquireCtx, cancel := context.WithTimeout(ctx, backend.lockTimeout)
	defer cancel()

	connection, err := backend.database.Conn(acquireCtx)
	if err != nil {
		return nil, databaseContextFailure(acquireCtx, "acquire dedicated PostgreSQL connection", err)
	}

	for {
		var acquired bool
		if err := connection.QueryRowContext(
			acquireCtx,
			"SELECT pg_try_advisory_lock($1)",
			advisoryLockKey,
		).Scan(&acquired); err != nil {
			_ = discardConnection(connection)
			if contextErr := acquireCtx.Err(); contextErr != nil {
				return nil, contextErr
			}

			return nil, databaseFailure("try PostgreSQL advisory lock", err)
		}
		if acquired {
			return &session{
				connection:       connection,
				statementTimeout: backend.statementTimeout,
				operationTimeout: backend.operationTimeout,
				gate:             newOperationGate(),
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
	connection       *sql.Conn
	statementTimeout time.Duration
	operationTimeout time.Duration
	gate             operationGate
	released         bool
	tainted          bool
}

type operationGate chan struct{}

func newOperationGate() operationGate {
	gate := make(operationGate, 1)
	gate <- struct{}{}

	return gate
}

func (gate operationGate) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
		return func() { gate <- struct{}{} }, nil
	}
}

func (session *session) beginOperation(
	ctx context.Context,
) (context.Context, func(), error) {
	operationCtx, cancel := boundedOperationContext(ctx, session.operationTimeout)
	if session.gate == nil {
		cancel()

		return nil, nil, ErrSessionReleased
	}
	release, err := session.gate.acquire(operationCtx)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	return operationCtx, func() {
		release()
		cancel()
	}, nil
}

func (session *session) Records(ctx context.Context) ([]migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if session.released || session.connection == nil {
		return nil, ErrSessionReleased
	}

	rows, err := session.connection.QueryContext(operationCtx, "SELECT kind, version, name, checksum, started_at, finished_at, execution_time_ms, dirty FROM public.migrations ORDER BY version ASC")
	if err != nil {
		return nil, databaseContextFailure(operationCtx, "query migration ledger", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]migrations.Record, 0, min(MaxLedgerRecords, 128))
	totalBytes := 0
	for rows.Next() {
		if len(records) >= MaxLedgerRecords {
			return nil, ErrResourceLimit
		}
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
			return nil, databaseContextFailure(
				operationCtx,
				"scan migration ledger row",
				errors.Join(migrations.ErrInvalidRecord, err),
			)
		}
		totalBytes, err = boundedTextBytes(totalBytes, MaxLedgerBytes, kindText, name, encoded)
		if err != nil {
			return nil, err
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
		return nil, databaseContextFailure(operationCtx, "iterate migration ledger rows", err)
	}

	return records, nil
}

func boundedTextBytes(total int, limit int, values ...string) (int, error) {
	for _, value := range values {
		if len(value) > limit-total {
			return 0, ErrResourceLimit
		}
		total += len(value)
	}

	return total, nil
}

func boundedOperationContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout < time.Millisecond {
		timeout = defaultOperationTimeout
	}

	return context.WithTimeout(ctx, timeout)
}

func (session *session) Apply(ctx context.Context, migration migrations.Migration) (migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return migrations.Record{}, err
	}
	defer finish()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}

	adapter, err := gooseadapter.Compile(migration)
	if err != nil {
		return migrations.Record{}, err
	}

	startedAt := time.Now().UTC()
	if migration.TransactionMode() == migrations.TransactionModeDefault {
		return session.applyTransaction(operationCtx, adapter, migration, startedAt)
	}

	return session.applyWithoutTransaction(operationCtx, adapter, migration, startedAt)
}

func (session *session) Rollback(ctx context.Context, migration migrations.Migration) (migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return migrations.Record{}, err
	}
	defer finish()
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
		return session.rollbackTransaction(operationCtx, adapter, migration)
	}

	return session.rollbackWithoutTransaction(operationCtx, adapter, migration)
}

// Recover persists an explicit operator-reviewed dirty outcome atomically.
func (session *session) Recover(
	ctx context.Context,
	migration migrations.Migration,
	action migrations.RecoveryAction,
) (migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return migrations.Record{}, err
	}
	defer finish()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}

	switch action {
	case migrations.RecoveryMarkApplied:
		finishedAt := time.Now().UTC()
		var durationMS int64
		err := session.connection.QueryRowContext(
			operationCtx,
			`UPDATE public.migrations SET finished_at = $1, execution_time_ms = GREATEST(0, floor(EXTRACT(EPOCH FROM ($1 - started_at)) * 1000)::bigint), dirty = false WHERE version = $2 AND checksum = $3 AND dirty = true AND GREATEST(0, floor(EXTRACT(EPOCH FROM ($1 - started_at)) * 1000)) <= $4 RETURNING execution_time_ms`,
			finishedAt,
			migrationLedgerVersion(migration),
			migration.Checksum().String(),
			maximumDurationMilliseconds,
		).Scan(&durationMS)
		if errors.Is(err, sql.ErrNoRows) {
			return migrations.Record{}, migrations.ErrNoDirtyMigration
		}
		if err != nil {
			return migrations.Record{}, databaseContextFailure(operationCtx, "mark dirty migration applied", err)
		}
		duration, err := durationFromMilliseconds(durationMS)
		if err != nil {
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
	case migrations.RecoveryMarkRolledBack:
		var startedAt time.Time
		var durationMS int64
		err := session.connection.QueryRowContext(
			operationCtx,
			`DELETE FROM public.migrations WHERE version = $1 AND checksum = $2 AND dirty = true AND execution_time_ms BETWEEN 0 AND $3 RETURNING started_at, execution_time_ms`,
			migrationLedgerVersion(migration),
			migration.Checksum().String(),
			maximumDurationMilliseconds,
		).Scan(&startedAt, &durationMS)
		if errors.Is(err, sql.ErrNoRows) {
			return migrations.Record{}, migrations.ErrNoDirtyMigration
		}
		if err != nil {
			return migrations.Record{}, databaseContextFailure(operationCtx, "remove rolled-back dirty migration", err)
		}

		duration, err := durationFromMilliseconds(durationMS)
		if err != nil {
			return migrations.Record{}, err
		}

		return migrations.NewRecord(
			migrations.RecordKindMigration,
			migration.Version(),
			migration.Name(),
			migration.Checksum(),
			startedAt,
			duration,
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
		return migrations.Record{}, databaseContextFailure(ctx, "begin rollback transaction", err)
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
		return migrations.Record{}, databaseContextFailure(ctx, "commit rollback transaction", err)
	}

	return record, nil
}

func setLocalStatementTimeout(
	ctx context.Context,
	transaction *sql.Tx,
	timeout time.Duration,
) error {
	_, err := transaction.ExecContext(
		ctx,
		"SELECT set_config('statement_timeout', $1, true)",
		strconv.FormatInt(timeout.Milliseconds(), 10)+"ms",
	)
	if err != nil {
		return databaseContextFailure(ctx, "set local migration statement timeout", err)
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
		`UPDATE public.migrations SET finished_at = NULL, dirty = true WHERE version = $1 AND checksum = $2 AND dirty = false AND execution_time_ms BETWEEN 0 AND $3 RETURNING started_at, execution_time_ms`,
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
		maximumDurationMilliseconds,
	).Scan(&appliedAt, &durationMS)
	if errors.Is(err, sql.ErrNoRows) {
		return migrations.Record{}, ErrLedgerConflict
	}
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "mark rollback dirty", err)
	}
	duration, err := durationFromMilliseconds(durationMS)
	if err != nil {
		return migrations.Record{}, err
	}
	if err := adapter.RollbackConn(ctx, session.connection); err != nil {
		return migrations.Record{}, err
	}
	result, err := session.connection.ExecContext(
		ctx,
		`DELETE FROM public.migrations WHERE version = $1 AND checksum = $2 AND dirty = true`,
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
	)
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "delete rolled-back migration record", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "read rolled-back migration result", err)
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
		duration,
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
		return migrations.Record{}, databaseContextFailure(ctx, "begin migration transaction", err)
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
		return migrations.Record{}, databaseContextFailure(ctx, "commit migration transaction", err)
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
		// The server may have changed session policy before its reply failed.
		session.tainted = true
		return databaseContextFailure(ctx, "set migration session statement timeout", err)
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
		"RESET statement_timeout",
	); err != nil {
		session.tainted = true
		return databaseContextFailure(resetCtx, "restore migration session statement timeout", err)
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
		`INSERT INTO public.migrations (version, kind, name, checksum, started_at, finished_at, execution_time_ms, dirty, engine, engine_version) VALUES ($1, $2, $3, $4, $5, NULL, 0, true, 'postgres', 'v1')`,
		migrationLedgerVersion(migration),
		"migration",
		migration.Name(),
		migration.Checksum().String(),
		startedAt,
	)
	if err != nil {
		return databaseContextFailure(ctx, "insert dirty migration record", err)
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
		`UPDATE public.migrations SET finished_at = $1, execution_time_ms = $2, dirty = false WHERE version = $3 AND checksum = $4 AND dirty = true`,
		finishedAt,
		duration.Milliseconds(),
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
	)
	if err != nil {
		return databaseContextFailure(ctx, "complete migration record", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return databaseContextFailure(ctx, "read completed migration result", err)
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
		`DELETE FROM public.migrations WHERE version = $1 AND checksum = $2 AND dirty = false RETURNING finished_at, execution_time_ms`,
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
	).Scan(&appliedAt, &durationMS)
	if errors.Is(err, sql.ErrNoRows) {
		return migrations.Record{}, ErrLedgerConflict
	}
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "delete rolled-back migration record", err)
	}
	duration, err := durationFromMilliseconds(durationMS)
	if err != nil {
		return migrations.Record{}, err
	}

	return migrations.NewRecord(
		migrations.RecordKindMigration,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		appliedAt,
		duration,
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
	if version < 1 {
		return migrations.Record{}, migrations.ErrInvalidRecord
	}
	duration, err := durationFromMilliseconds(durationMS)
	if err != nil {
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
		duration,
		dirty,
	)
	if err != nil {
		return migrations.Record{}, fmt.Errorf("%w: %w", migrations.ErrInvalidRecord, err)
	}

	return record, nil
}

func durationFromMilliseconds(durationMS int64) (time.Duration, error) {
	if durationMS < 0 || durationMS > maximumDurationMilliseconds {
		return 0, migrations.ErrInvalidRecord
	}

	return time.Duration(durationMS) * time.Millisecond, nil
}

func migrationLedgerVersion(migration migrations.Migration) int64 {
	// #nosec G115 -- Migration has private fields and NewMigration rejects
	// versions above math.MaxInt64 before PostgreSQL persistence can receive one.
	return int64(migration.Version())
}

func (session *session) Release(ctx context.Context) error {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if session.released || session.connection == nil {
		return ErrSessionReleased
	}

	var unlocked bool
	queryErr := session.connection.QueryRowContext(
		operationCtx,
		"SELECT pg_advisory_unlock($1)",
		advisoryLockKey,
	).Scan(&unlocked)
	var closeErr error
	if queryErr != nil || session.tainted {
		closeErr = discardConnection(session.connection)
	} else {
		closeErr = session.connection.Close()
	}
	session.released = true
	session.connection = nil
	if queryErr != nil {
		return errors.Join(
			databaseContextFailure(operationCtx, "release PostgreSQL advisory lock", queryErr),
			databaseFailure("close PostgreSQL migration connection", closeErr),
		)
	}
	if !unlocked {
		return errors.Join(
			ErrLockNotHeld,
			databaseFailure("close PostgreSQL migration connection", closeErr),
		)
	}

	return databaseFailure("close PostgreSQL migration connection", closeErr)
}

// A failed lock query cannot establish the server's lock state. Close alone
// returns the physical session to the pool; ErrBadConn makes sql discard it.
func discardConnection(connection *sql.Conn) error {
	err := connection.Raw(func(any) error { return driver.ErrBadConn })
	closeErr := connection.Close()
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		err = nil
	}
	if errors.Is(closeErr, sql.ErrConnDone) {
		closeErr = nil
	}
	return errors.Join(err, closeErr)
}
