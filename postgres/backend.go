// Package postgres implements the public.migrations ledger and connection-bound
// advisory-lock backend for migrations.Runner. New borrows a caller-owned
// *pgxpool.Pool; it does not open, close or migrate the database at
// construction. Acquire owns one leased connection until Release. Cooperating
// jobs in the same database share a stable lock; unrelated schema writers are
// not excluded.
//
// Transactional SQL and ledger changes use one transaction per migration.
// Nontransactional execution persists dirty state first; unresolved dirty
// outcomes require explicit operator review/recovery. An error during commit or timeout cleanup
// can follow durable changes; inspect state before retrying. Finite contexts
// bound work; rollback and connection close use detached finite deadlines.
//
// Default PostgreSQL operation formatting hides driver diagnostics, but error
// traversal may expose the original cause. Schema inspection returns sensitive
// definitions and covers only selected catalog properties, not a security audit.
// See docs/operations.md and docs/threat-model.md (risks 003, 006 and 007).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	advisoryLockKey int64 = 0x676f6d6967726174
	// MaxLedgerRecords permits one baseline plus the complete bounded migration
	// source while preventing a hostile ledger from growing retained state.
	MaxLedgerRecords = migrations.MaxMigrationFiles + 1
	// MaxLedgerBytes bounds aggregate kind/name/checksum text retained from one
	// ledger read, not allocations made by the driver before each scan.
	MaxLedgerBytes = 16 << 20
	// Literal nanoseconds keep these safety budgets indivisible while retaining
	// their exact documented durations.
	defaultLockRetryInterval   time.Duration = 100_000_000
	defaultLockTimeout         time.Duration = 30_000_000_000
	defaultStatementTimeout    time.Duration = 300_000_000_000
	defaultOperationTimeout    time.Duration = 600_000_000_000
	statementTimeoutResetLimit time.Duration = 30_000_000_000
	// finalizationTimeout bounds rollback and physical close after the
	// operation context ended; cleanup must still run but never hang forever.
	finalizationTimeout         time.Duration = 30_000_000_000
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

// Error returns only the fixed operation and classification, not the cause.
func (err *databaseError) Error() string {
	return err.operation + ": " + ErrDatabaseOperationFailed.Error()
}

// Format keeps even expanded formatting redacted; quoted formatting quotes
// Error text without inspecting the driver cause.
func (err *databaseError) Format(state fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(state, "%q", err.Error())

		return
	}

	_, _ = fmt.Fprint(state, err.Error())
}

// Is exposes the operation category without adding a synthetic failure cause.
func (*databaseError) Is(target error) bool { return target == ErrDatabaseOperationFailed }

// Unwrap exposes the original driver cause to errors.Is/As. This remains an
// explicit sensitive-diagnostic boundary, not a redacted cause chain.
func (err *databaseError) Unwrap() error {
	return err.cause
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

// Ledger creation runs on the advisory-lock connection before any history reads.
const createLedgerSQL = `CREATE TABLE IF NOT EXISTS public.migrations (
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

// Option configures a PostgreSQL backend during construction only.
type Option func(*Backend) error

// WithLockRetryInterval controls polling when another job owns the advisory
// lock. Interval must be positive; the default is 100 milliseconds. Context
// cancellation interrupts the timer between unsuccessful contention attempts.
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
	pool              connectionPool
	namespace         string
	lockRetryInterval time.Duration
	lockTimeout       time.Duration
	statementTimeout  time.Duration
	operationTimeout  time.Duration
}

// New constructs the backend without using pool itself or taking its
// ownership. Caller options run left-to-right until a nil/failing option; repeated
// settings use the last value. Defaults are 30-second acquisition, five-minute
// statement and ten-minute primary-operation timeouts. Each session leases one
// pool connection for its lifetime, so size the pool for concurrent sessions
// plus inspection and discovery. The caller closes the pool after all sessions
// finish and must not mutate options during use.
func New(pool *pgxpool.Pool, options ...Option) (*Backend, error) {
	if pool == nil {
		return nil, ErrInvalidConfig
	}

	return newBackend(pgxPool{pool}, options...)
}

func newBackend(pool connectionPool, options ...Option) (*Backend, error) {
	backend := &Backend{
		pool:              pool,
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

// Prepare creates the selected ledger if absent on the advisory-lock connection.
// Existing Laravel history must be relocated first.
func (session *session) Prepare(ctx context.Context) error {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if session.released || session.connection == nil {
		return ErrSessionReleased
	}
	if _, err := session.connection.Exec(operationCtx, ledgerSQL(createLedgerSQL, session.namespace)); err != nil {
		return databaseContextFailure(operationCtx, "create migration ledger", err)
	}

	return nil
}

// Acquire waits for the stable package advisory lock on one physical
// connection and returns all subsequent session operations bound to it. Its
// lock timeout includes pool acquisition and polling. Database failures are not
// retried; uncertain lock-query failures discard the physical connection.
// A successful caller must Release the session even after operation failure.
func (backend *Backend) Acquire(ctx context.Context) (migrations.Session, error) {
	if backend == nil || backend.pool == nil {
		return nil, ErrInvalidConfig
	}

	acquireCtx, cancel := context.WithTimeout(ctx, backend.lockTimeout)
	defer cancel()

	connection, err := backend.pool.acquire(acquireCtx)
	if err != nil {
		return nil, databaseContextFailure(acquireCtx, "acquire dedicated PostgreSQL connection", err)
	}

	for {
		var acquired bool
		if err := connection.QueryRow(
			acquireCtx,
			"SELECT pg_try_advisory_lock($1)",
			advisoryLockKey,
		).Scan(&acquired); err != nil {
			_ = discardConnection(acquireCtx, connection)
			return nil, databaseContextFailure(acquireCtx, "try PostgreSQL advisory lock", err)
		}
		if acquired {
			return &session{
				connection:       connection,
				namespace:        backend.namespace,
				statementTimeout: backend.statementTimeout,
				operationTimeout: backend.operationTimeout,
				gate:             newOperationGate(),
			}, nil
		}

		timer := time.NewTimer(backend.lockRetryInterval)
		select {
		case <-acquireCtx.Done():
			timer.Stop()
			connection.release()

			return nil, acquireCtx.Err()
		case <-timer.C:
		}
	}
}

type session struct {
	connection       connection
	namespace        string
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

// The gate owns released/tainted state and the leased connection for the whole
// operation. Timeout includes waiting for this ownership, not just SQL time.
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

// Records reads ordered history with bounded retained count/text, validating
// durations and clean/dirty completion consistency before returning values.
func (session *session) Records(ctx context.Context) ([]migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if session.released || session.connection == nil {
		return nil, ErrSessionReleased
	}

	rows, err := session.connection.Query(operationCtx, ledgerSQL(ledgerRecordsSQL, session.namespace))
	if err != nil {
		return nil, databaseContextFailure(operationCtx, "query migration ledger", err)
	}
	defer rows.Close()
	return decodeLedgerRows(operationCtx, rows)
}

// The caller closes rows; decoding stops at the first invalid or excess row.
func decodeLedgerRows(operationCtx context.Context, rows pgx.Rows) ([]migrations.Record, error) {
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
			finishedAt pgtype.Timestamptz
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
		nextBytes, err := boundedTextBytes(totalBytes, MaxLedgerBytes, kindText, name, encoded)
		if err != nil {
			return nil, err
		}
		totalBytes = nextBytes
		if dirty == finishedAt.Valid || finishedAt.InfinityModifier != pgtype.Finite {
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

// Apply commits one migration under its transaction policy. A timeout-reset
// error can accompany already completed nontransactional SQL and a clean row.
// Transactional failures retain explicit rollback errors; Release discards a
// physical session whose rollback outcome is uncertain.
func (session *session) Apply(ctx context.Context, migration migrations.Migration) (migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return migrations.Record{}, err
	}
	defer finish()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}

	if err := validMigration(migration); err != nil {
		return migrations.Record{}, err
	}

	startedAt := time.Now().UTC()
	if migration.TransactionMode() == migrations.TransactionModeDefault {
		return session.applyTransaction(operationCtx, migration, startedAt)
	}

	return session.applyWithoutTransaction(operationCtx, migration, startedAt)
}

// Rollback removes one clean row after down SQL under its transaction policy.
// Nontransactional execution marks it dirty before SQL, not after a failure.
// Transactional failures retain explicit cleanup errors for the same reason as Apply.
func (session *session) Rollback(ctx context.Context, migration migrations.Migration) (migrations.Record, error) {
	operationCtx, finish, err := session.beginOperation(ctx)
	if err != nil {
		return migrations.Record{}, err
	}
	defer finish()
	if session.released || session.connection == nil {
		return migrations.Record{}, ErrSessionReleased
	}
	if err := validMigration(migration); err != nil {
		return migrations.Record{}, err
	}
	if migration.DownSQL() == "" {
		return migrations.Record{}, migrations.ErrIrreversible
	}
	if migration.TransactionMode() == migrations.TransactionModeDefault {
		return session.rollbackTransaction(operationCtx, migration)
	}

	return session.rollbackWithoutTransaction(operationCtx, migration)
}

// Recover persists an explicit operator-reviewed dirty outcome atomically.
// It changes only the checksum-matched dirty ledger row, without inspecting or
// executing migration SQL. MarkRolledBack returns the removed dirty record.
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
		err := session.connection.QueryRow(
			operationCtx,
			ledgerSQL(`UPDATE public.migrations SET finished_at = $1, execution_time_ms = GREATEST(0, floor(EXTRACT(EPOCH FROM ($1 - started_at)) * 1000)::bigint), dirty = false WHERE version = $2 AND checksum = $3 AND dirty = true AND GREATEST(0, floor(EXTRACT(EPOCH FROM ($1 - started_at)) * 1000)) <= $4 RETURNING execution_time_ms`, session.namespace),
			finishedAt,
			migrationLedgerVersion(migration),
			migration.Checksum().String(),
			maximumDurationMilliseconds,
		).Scan(&durationMS)
		if errors.Is(err, pgx.ErrNoRows) {
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
		err := session.connection.QueryRow(
			operationCtx,
			ledgerSQL(`DELETE FROM public.migrations WHERE version = $1 AND checksum = $2 AND dirty = true AND execution_time_ms BETWEEN 0 AND $3 RETURNING started_at, execution_time_ms`, session.namespace),
			migrationLedgerVersion(migration),
			migration.Checksum().String(),
			maximumDurationMilliseconds,
		).Scan(&startedAt, &durationMS)
		if errors.Is(err, pgx.ErrNoRows) {
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
	migration migrations.Migration,
) (record migrations.Record, err error) {
	transaction, err := session.connection.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "begin rollback transaction", err)
	}
	defer session.rollbackTransactionCleanup(ctx, transaction, &err)
	if err := setLocalStatementTimeout(ctx, transaction, session.statementTimeout); err != nil {
		return migrations.Record{}, err
	}

	if err := executeMigrationSQL(ctx, transaction, migration.DownSQL()); err != nil {
		return migrations.Record{}, err
	}
	record, err = deleteRecord(ctx, transaction, migration, session.namespace)
	if err != nil {
		return migrations.Record{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "commit rollback transaction", err)
	}

	return record, nil
}

func setLocalStatementTimeout(
	ctx context.Context,
	transaction queryer,
	timeout time.Duration,
) error {
	_, err := transaction.Exec(
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
	err = session.connection.QueryRow(
		ctx,
		ledgerSQL(`UPDATE public.migrations SET finished_at = NULL, dirty = true WHERE version = $1 AND checksum = $2 AND dirty = false AND execution_time_ms BETWEEN 0 AND $3 RETURNING started_at, execution_time_ms`, session.namespace),
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
		maximumDurationMilliseconds,
	).Scan(&appliedAt, &durationMS)
	if errors.Is(err, pgx.ErrNoRows) {
		return migrations.Record{}, ErrLedgerConflict
	}
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "mark rollback dirty", err)
	}
	duration, err := durationFromMilliseconds(durationMS)
	if err != nil {
		return migrations.Record{}, err
	}
	if err := executeMigrationSQL(ctx, session.connection, migration.DownSQL()); err != nil {
		return migrations.Record{}, err
	}
	result, err := session.connection.Exec(
		ctx,
		ledgerSQL(`DELETE FROM public.migrations WHERE version = $1 AND checksum = $2 AND dirty = true`, session.namespace),
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
	)
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "delete rolled-back migration record", err)
	}
	if result.RowsAffected() != 1 {
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
	migration migrations.Migration,
	startedAt time.Time,
) (record migrations.Record, err error) {
	transaction, err := session.connection.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return migrations.Record{}, databaseContextFailure(ctx, "begin migration transaction", err)
	}
	defer session.rollbackTransactionCleanup(ctx, transaction, &err)
	if err := setLocalStatementTimeout(ctx, transaction, session.statementTimeout); err != nil {
		return migrations.Record{}, err
	}

	if err := insertDirty(ctx, transaction, migration, startedAt, session.namespace); err != nil {
		return migrations.Record{}, err
	}
	if err := executeMigrationSQL(ctx, transaction, migration.UpSQL()); err != nil {
		return migrations.Record{}, err
	}

	finishedAt := time.Now().UTC()
	duration := finishedAt.Sub(startedAt)
	if err := markClean(ctx, transaction, migration, finishedAt, duration, session.namespace); err != nil {
		return migrations.Record{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
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

func (session *session) rollbackTransactionCleanup(ctx context.Context, transaction transaction, result *error) {
	if err := rollbackTransaction(ctx, transaction); err != nil {
		// pgx closes the connection after a failed rollback; the server outcome
		// is then unknown, so Release must discard rather than reuse it.
		session.tainted = true
		*result = errors.Join(*result, databaseFailure("rollback migration transaction", err))
	}
}

// rollbackTransaction finalizes on a detached finite deadline: caller
// cancellation must not skip the rollback that returns the session to idle.
// ErrTxClosed means Commit already finished the transaction.
func rollbackTransaction(ctx context.Context, transaction transaction) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := transaction.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}

	return nil
}

func (session *session) applyWithoutTransaction(
	ctx context.Context,
	migration migrations.Migration,
	startedAt time.Time,
) (record migrations.Record, err error) {
	if err = session.setSessionStatementTimeout(ctx); err != nil {
		return migrations.Record{}, err
	}
	defer func() {
		err = errors.Join(err, session.resetSessionStatementTimeout(ctx))
	}()

	if err := insertDirty(ctx, session.connection, migration, startedAt, session.namespace); err != nil {
		return migrations.Record{}, err
	}
	if err := executeMigrationSQL(ctx, session.connection, migration.UpSQL()); err != nil {
		return migrations.Record{}, err
	}

	finishedAt := time.Now().UTC()
	duration := finishedAt.Sub(startedAt)
	if err := markClean(ctx, session.connection, migration, finishedAt, duration, session.namespace); err != nil {
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
	_, err := session.connection.Exec(
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

// Restore database/role policy, not a caller's prior session-specific override.
// Detach cleanup from cancellation; uncertainty taints the connection so Release
// discards it even if advisory unlock succeeds.
func (session *session) resetSessionStatementTimeout(ctx context.Context) error {
	if session.statementTimeout == 0 {
		return nil
	}
	resetCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		statementTimeoutResetLimit,
	)
	defer cancel()
	if _, err := session.connection.Exec(
		resetCtx,
		"RESET statement_timeout",
	); err != nil {
		session.tainted = true
		return databaseContextFailure(resetCtx, "restore migration session statement timeout", err)
	}

	return nil
}

func insertDirty(
	ctx context.Context,
	execer queryer,
	migration migrations.Migration,
	startedAt time.Time,
	namespace string,
) error {
	_, err := execer.Exec(
		ctx,
		ledgerSQL(`INSERT INTO public.migrations (version, kind, name, checksum, started_at, finished_at, execution_time_ms, dirty, engine, engine_version) VALUES ($1, $2, $3, $4, $5, NULL, 0, true, 'postgres', 'v1')`, namespace),
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
	execer queryer,
	migration migrations.Migration,
	finishedAt time.Time,
	duration time.Duration,
	namespace string,
) error {
	result, err := execer.Exec(
		ctx,
		ledgerSQL(`UPDATE public.migrations SET finished_at = $1, execution_time_ms = $2, dirty = false WHERE version = $3 AND checksum = $4 AND dirty = true`, namespace),
		finishedAt,
		duration.Milliseconds(),
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
	)
	if err != nil {
		return databaseContextFailure(ctx, "complete migration record", err)
	}
	if result.RowsAffected() != 1 {
		return ErrLedgerConflict
	}

	return nil
}

func deleteRecord(
	ctx context.Context,
	transaction queryer,
	migration migrations.Migration,
	namespace string,
) (migrations.Record, error) {
	var appliedAt time.Time
	var durationMS int64
	err := transaction.QueryRow(
		ctx,
		ledgerSQL(`DELETE FROM public.migrations WHERE version = $1 AND checksum = $2 AND dirty = false RETURNING finished_at, execution_time_ms`, namespace),
		migrationLedgerVersion(migration),
		migration.Checksum().String(),
	).Scan(&appliedAt, &durationMS)
	if errors.Is(err, pgx.ErrNoRows) {
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

// Release waits for operation ownership before touching the connection. If
// canceled while waiting it leaves the active session intact. Once owned, it
// attempts unlock, returns healthy connections to the pool and discards tainted
// or uncertain ones. A repeated release returns ErrSessionReleased, not success.
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
	queryErr := session.connection.QueryRow(
		operationCtx,
		"SELECT pg_advisory_unlock($1)",
		advisoryLockKey,
	).Scan(&unlocked)
	var closeErr error
	if queryErr != nil || session.tainted {
		closeErr = discardConnection(operationCtx, session.connection)
	} else {
		session.connection.release()
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

// A failed lock query cannot establish the server's lock state, so the
// physical session is closed rather than released to the pool. Closing ends
// the server session, which drops its advisory locks and session settings.
// The close runs on a detached finite deadline because the operation context
// may already have ended.
func discardConnection(ctx context.Context, connection connection) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()

	return connection.discard(cleanupCtx)
}
