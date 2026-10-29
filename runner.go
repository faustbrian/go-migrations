package migrations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

const defaultUnlockTimeout time.Duration = 30_000_000_000

var (
	// ErrInvalidRunner indicates missing or inconsistent runner dependencies.
	ErrInvalidRunner = errors.New("invalid migration runner")
	// ErrBackendResult indicates an engine violated the public backend contract.
	ErrBackendResult = errors.New("invalid migration backend result")
	// ErrExecutionFailed identifies migration SQL execution failure without
	// exposing database-driver diagnostics or migration contents.
	ErrExecutionFailed = errors.New("migration SQL execution failed")
)

// Session represents exclusive migration-job ownership bound to one physical
// database connection. Ledger preparation, reads, and execution all happen
// through this value so connection loss and lock loss cannot be separated from
// execution and a one-connection pool cannot deadlock during preparation.
// Records must return at most MaxMigrationRecords entries.
type Session interface {
	// Prepare initializes the owned ledger on the lock-owning connection. It
	// may execute SQL even for a plan/status operation.
	Prepare(context.Context) error
	// Records returns the complete, version-ordered ledger within
	// MaxMigrationRecords, with returned data stable for the caller to validate.
	Records(context.Context) ([]Record, error)
	// Apply executes up SQL and persists its matching clean record on success.
	// Transactional SQL and ledger writes must be atomic; nontransactional work
	// must persist dirty state before executing SQL. Errors can follow durable
	// changes and must not be interpreted as proof that nothing ran.
	Apply(context.Context, Migration) (Record, error)
	// Rollback executes down SQL and removes the matching record on success,
	// returning the removed clean record. It has the same transactional and
	// explicit dirty-outcome obligations as Apply.
	Rollback(context.Context, Migration) (Record, error)
	// Release relinquishes the session's connection and exclusive ownership.
	// Runner attempts it once after acquisition using a detached finite context;
	// cleanup errors are joined with any operation error.
	Release(context.Context) error
}

// Backend is the replaceable migration-engine conformance boundary.
//
// Session.Apply must return only after both execution and owned-ledger
// persistence have reached an explicit recoverable outcome. Implementations
// must leave a dirty record when non-transactional execution has an uncertain
// or partial result. Every operation must honor its context and return bounded
// results required by Session.
type Backend interface {
	// Acquire returns a nonnil exclusively owned session or an error. The
	// backend must serialize cooperating jobs, honor cancellation and ensure an
	// unsuccessful acquisition does not leave an unmanaged locked connection.
	Acquire(context.Context) (Session, error)
}

// Operation identifies an observable migration lifecycle operation.
type Operation uint8

const (
	// OperationLock acquires exclusive migration-job ownership.
	OperationLock Operation = iota + 1
	// OperationApply applies one migration and persists its outcome.
	OperationApply
	// OperationRollback rolls back one migration and removes its ledger record.
	OperationRollback
	// OperationBaseline validates and records one reviewed existing schema.
	OperationBaseline
	// OperationRecover resolves one explicitly reviewed dirty outcome.
	OperationRecover
	// OperationUnlock releases exclusive migration-job ownership.
	OperationUnlock
)

// Phase identifies the lifecycle phase of an emitted operation.
type Phase uint8

const (
	// PhaseStarted is emitted immediately before an operation.
	PhaseStarted Phase = iota + 1
	// PhaseCompleted is emitted after a successful operation.
	PhaseCompleted
	// PhaseFailed is emitted after a failed operation.
	PhaseFailed
)

// Event is an immutable structured diagnostic suitable for logging and tracing
// adapters. There is no SQL text field, but an error from a custom dependency
// can contain sensitive data. The runner does not sanitize every error cause.
type Event struct {
	operation Operation
	phase     Phase
	version   Version
	duration  time.Duration
	err       error
}

// Operation returns the lifecycle operation.
func (event Event) Operation() Operation { return event.operation }

// Phase returns the lifecycle phase.
func (event Event) Phase() Phase { return event.phase }

// Version returns the affected migration version, or zero for job operations.
func (event Event) Version() Version { return event.version }

// Duration returns elapsed time for completed or failed operations.
func (event Event) Duration() time.Duration { return event.duration }

// Err returns the operation error, except that baseline-mismatch observer
// events use the classification alone. PostgreSQL operation errors redact
// ordinary formatting but can expose driver causes through error traversal;
// custom backend errors are retained. Do not log unwrapped causes.
func (event Event) Err() error { return event.err }

// Observer consumes synchronous migration events. Shared observers must be
// concurrency-safe and bounded/nonblocking; they are trusted caller code.
type Observer interface {
	// Observe receives an event inline. Panics are contained, but blocking is
	// not preempted. Cleanup events use a context detached from cancellation;
	// observers need their own finite bound for that work.
	Observe(context.Context, Event)
}

// Result describes the successful acknowledged prefix of one runner Up/Down
// invocation, including on error. It is not a whole-batch transaction receipt:
// the failing step or cleanup may also have changed durable database state.
type Result struct {
	records []Record
}

// Records returns a copy of acknowledged records in execution order. Down
// returns the historical clean records that were removed, not surviving rows.
func (result Result) Records() []Record {
	return append([]Record(nil), result.records...)
}

// Option configures a Runner during construction only.
type Option func(*Runner) error

// WithObserver installs a structured event observer. Observer panics are
// contained; callbacks still run synchronously and can delay work or cleanup.
// A nil interface is rejected; the caller must ensure any typed value is usable.
func WithObserver(observer Observer) Option {
	return func(runner *Runner) error {
		if observer == nil {
			return ErrInvalidRunner
		}
		runner.observer = observer

		return nil
	}
}

// WithUnlockTimeout replaces the default 30-second, best-effort release
// deadline with a positive duration. Release uses context.WithoutCancel so
// caller cancellation does not skip its attempt. It cannot forcibly bound a
// blocking observer or a backend that ignores its context.
func WithUnlockTimeout(timeout time.Duration) Option {
	return func(runner *Runner) error {
		if timeout <= 0 {
			return ErrInvalidRunner
		}
		runner.unlockTimeout = timeout

		return nil
	}
}

// Runner coordinates source validation, exclusive locking, history
// revalidation, planning, and backend execution.
type Runner struct {
	source        Source
	backend       Backend
	observer      Observer
	unlockTimeout time.Duration
}

// NewRunner borrows source/backend without loading or acquiring them itself.
// Options are caller code. It rejects nil interfaces/options, applies options left-to-right
// until failure and starts no goroutines. Typed-nil dependencies and arbitrary
// callback/backend panics are not generally validated or contained. Operations
// require a nonnil context and usable dependencies.
func NewRunner(source Source, backend Backend, options ...Option) (*Runner, error) {
	if source == nil || backend == nil {
		return nil, ErrInvalidRunner
	}

	runner := &Runner{
		source:        source,
		backend:       backend,
		unlockTimeout: defaultUnlockTimeout,
	}
	for _, option := range options {
		if option == nil {
			return nil, ErrInvalidRunner
		}
		if err := option(runner); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRunner, err)
		}
	}

	return runner, nil
}

// Status loads source before locking, prepares/reads the ledger under the
// session lock and returns their validated status. It does not inspect live SQL
// effects or freeze source changes; ledger preparation may execute SQL.
func (runner *Runner) Status(ctx context.Context) (status Status, err error) {
	if runner == nil || runner.source == nil || runner.backend == nil {
		return Status{}, ErrInvalidRunner
	}
	available, err := runner.source.Load(ctx)
	if err != nil {
		return Status{}, err
	}
	err = runner.withSession(ctx, func(session Session) error {
		records, recordsErr := session.Records(ctx)
		if recordsErr != nil {
			return fmt.Errorf("read migration records: %w", recordsErr)
		}
		status, recordsErr = BuildStatus(available, records)

		return recordsErr
	})

	return status, err
}

// Plan loads source before locking and validates that history against the
// locked ledger without executing migration SQL. It can initialize the ledger;
// it is not an I/O-free operation or a reservation for a later Up call.
func (runner *Runner) Plan(ctx context.Context) (plan Plan, err error) {
	if runner == nil || runner.source == nil || runner.backend == nil {
		return Plan{}, ErrInvalidRunner
	}
	available, err := runner.source.Load(ctx)
	if err != nil {
		return Plan{}, err
	}
	err = runner.withSession(ctx, func(session Session) error {
		records, recordsErr := session.Records(ctx)
		if recordsErr != nil {
			return fmt.Errorf("read migration records: %w", recordsErr)
		}
		plan, recordsErr = PlanUp(available, records)

		return recordsErr
	})

	return plan, err
}

type recoverySession interface {
	Session
	Recover(context.Context, Migration, RecoveryAction) (Record, error)
}

// Recover resolves one checksum-matched dirty migration after the operator has
// verified that its effects are either fully applied or fully removed. The
// session must support the Recover method taking context.Context, Migration
// and RecoveryAction and returning (Record, error), otherwise
// ErrRecoveryUnsupported is returned. The runner checks source and
// dirty-record identity but does not execute SQL or verify the asserted effects.
// A release error can follow a persisted recovery decision.
func (runner *Runner) Recover(
	ctx context.Context,
	recovery Recovery,
) (result RecoveryResult, err error) {
	if runner == nil || runner.source == nil || runner.backend == nil {
		return RecoveryResult{}, ErrInvalidRunner
	}
	if recovery.Version() == 0 || recovery.Checksum() == (Checksum{}) ||
		(recovery.Action() != RecoveryMarkApplied && recovery.Action() != RecoveryMarkRolledBack) {
		return RecoveryResult{}, ErrInvalidRecovery
	}

	available, err := runner.source.Load(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	if err := validateAvailableOrder(available); err != nil {
		return RecoveryResult{}, err
	}
	migrationIndex := slices.IndexFunc(available, func(candidate Migration) bool {
		return candidate.Version() == recovery.Version()
	})
	var migration Migration
	if migrationIndex >= 0 {
		migration = available[migrationIndex]
	}
	if migration.Version() == 0 || migration.Checksum() != recovery.Checksum() {
		return RecoveryResult{}, ErrRecoveryMismatch
	}

	err = runner.withSession(ctx, func(session Session) error {
		records, recordsErr := session.Records(ctx)
		if recordsErr != nil {
			return fmt.Errorf("read migration records: %w", recordsErr)
		}
		status, statusErr := BuildStatus(available, records)
		if statusErr != nil {
			return statusErr
		}
		dirtyCount := 0
		matchingDirty := false
		for _, entry := range status.Entries() {
			if entry.State() != StateDirty {
				continue
			}
			dirtyCount++
			if entry.Version() == recovery.Version() {
				matchingDirty = true
			}
		}
		if dirtyCount > 1 {
			return ErrRecoveryConflict
		}
		if !matchingDirty {
			return ErrNoDirtyMigration
		}

		capable, ok := session.(recoverySession)
		if !ok {
			return ErrRecoveryUnsupported
		}
		started := time.Now()
		runner.observe(ctx, Event{
			operation: OperationRecover,
			phase:     PhaseStarted,
			version:   migration.Version(),
		})
		record, recoverErr := capable.Recover(ctx, migration, recovery.Action())
		if recoverErr != nil {
			runner.observe(ctx, Event{
				operation: OperationRecover,
				phase:     PhaseFailed,
				version:   migration.Version(),
				duration:  elapsed(started),
				err:       recoverErr,
			})

			return fmt.Errorf("recover migration %d_%s: %w", migration.Version(), migration.Name(), recoverErr)
		}
		if record.Kind() != RecordKindMigration ||
			record.Version() != migration.Version() ||
			record.Name() != migration.Name() ||
			record.Checksum() != migration.Checksum() ||
			record.AppliedAt().IsZero() ||
			(recovery.Action() == RecoveryMarkApplied && record.Dirty()) ||
			(recovery.Action() == RecoveryMarkRolledBack && !record.Dirty()) {
			return fmt.Errorf("%w: recovery version %d", ErrBackendResult, migration.Version())
		}
		result = RecoveryResult{action: recovery.Action(), record: record}
		runner.observe(ctx, Event{
			operation: OperationRecover,
			phase:     PhaseCompleted,
			version:   migration.Version(),
			duration:  elapsed(started),
		})

		return nil
	})

	return result, err
}

// Source has already been loaded when this lock is acquired. Keep preparation
// on the session itself to avoid leasing a second connection from a one-slot
// pool. A cleanup failure must not erase the primary operation's outcome.
func (runner *Runner) withSession(ctx context.Context, operation func(Session) error) (err error) {
	started := time.Now()
	runner.observe(ctx, Event{operation: OperationLock, phase: PhaseStarted})
	session, err := runner.backend.Acquire(ctx)
	if err != nil {
		runner.observe(ctx, Event{
			operation: OperationLock,
			phase:     PhaseFailed,
			duration:  elapsed(started),
			err:       err,
		})

		return fmt.Errorf("acquire migration lock: %w", err)
	}
	runner.observe(ctx, Event{
		operation: OperationLock,
		phase:     PhaseCompleted,
		duration:  elapsed(started),
	})

	defer func() {
		unlockStarted := time.Now()
		runner.observe(context.WithoutCancel(ctx), Event{
			operation: OperationUnlock,
			phase:     PhaseStarted,
		})
		releaseCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			runner.unlockTimeout,
		)
		defer cancel()
		if releaseErr := session.Release(releaseCtx); releaseErr != nil {
			runner.observe(context.WithoutCancel(ctx), Event{
				operation: OperationUnlock,
				phase:     PhaseFailed,
				duration:  elapsed(unlockStarted),
				err:       releaseErr,
			})
			err = errors.Join(err, fmt.Errorf("release migration lock: %w", releaseErr))

			return
		}
		runner.observe(context.WithoutCancel(ctx), Event{
			operation: OperationUnlock,
			phase:     PhaseCompleted,
			duration:  elapsed(unlockStarted),
		})
	}()
	if err = session.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare migration backend: %w", err)
	}

	return operation(session)
}

// Up loads source, acquires a session and validates that loaded history against
// the locked ledger before applying pending steps in ascending order. The lock
// coordinates backend jobs, not source mutations or unrelated database writers.
// Each step has its own backend transaction policy, not a batch transaction.
// On failure Result contains only earlier acknowledged steps; inspect database
// and ledger state before retry because the failing step or cleanup may have
// followed durable changes. The runner does not retry automatically.
func (runner *Runner) Up(ctx context.Context) (result Result, err error) {
	if runner == nil || runner.source == nil || runner.backend == nil {
		return Result{}, ErrInvalidRunner
	}

	available, err := runner.source.Load(ctx)
	if err != nil {
		return Result{}, err
	}
	err = runner.withSession(ctx, func(session Session) error {
		records, recordsErr := session.Records(ctx)
		if recordsErr != nil {
			return fmt.Errorf("read migration records: %w", recordsErr)
		}
		plan, planErr := PlanUp(available, records)
		if planErr != nil {
			return planErr
		}

		result.records = make([]Record, 0, len(plan.steps))
		for _, step := range plan.steps {
			migration := step.Migration()
			started := time.Now()
			runner.observe(ctx, Event{
				operation: OperationApply,
				phase:     PhaseStarted,
				version:   migration.Version(),
			})

			record, applyErr := session.Apply(ctx, migration)
			if applyErr != nil {
				runner.observe(ctx, Event{
					operation: OperationApply,
					phase:     PhaseFailed,
					version:   migration.Version(),
					duration:  elapsed(started),
					err:       applyErr,
				})

				return fmt.Errorf("apply migration %d_%s: %w", migration.Version(), migration.Name(), applyErr)
			}
			if recordErr := validateBackendRecord(migration, record); recordErr != nil {
				return recordErr
			}

			result.records = append(result.records, record)
			runner.observe(ctx, Event{
				operation: OperationApply,
				phase:     PhaseCompleted,
				version:   migration.Version(),
				duration:  elapsed(started),
			})
		}

		return nil
	})

	return result, err
}

// Down plans exactly count clean applied migrations, newest first, validating
// loaded source against locked ledger history. Count must be positive; planning
// fails before SQL if any selected migration is irreversible or crosses a
// baseline. Execution can stop partway: Result contains acknowledged removals,
// and an error does not establish that the failing step made no durable change.
func (runner *Runner) Down(ctx context.Context, count uint64) (result Result, err error) {
	if runner == nil || runner.source == nil || runner.backend == nil {
		return Result{}, ErrInvalidRunner
	}

	available, err := runner.source.Load(ctx)
	if err != nil {
		return Result{}, err
	}
	err = runner.withSession(ctx, func(session Session) error {
		records, recordsErr := session.Records(ctx)
		if recordsErr != nil {
			return fmt.Errorf("read migration records: %w", recordsErr)
		}
		plan, planErr := PlanDown(available, records, count)
		if planErr != nil {
			return planErr
		}

		result.records = make([]Record, 0, len(plan.steps))
		for _, step := range plan.steps {
			migration := step.Migration()
			started := time.Now()
			runner.observe(ctx, Event{
				operation: OperationRollback,
				phase:     PhaseStarted,
				version:   migration.Version(),
			})

			record, rollbackErr := session.Rollback(ctx, migration)
			if rollbackErr != nil {
				runner.observe(ctx, Event{
					operation: OperationRollback,
					phase:     PhaseFailed,
					version:   migration.Version(),
					duration:  elapsed(started),
					err:       rollbackErr,
				})

				return fmt.Errorf("rollback migration %d_%s: %w", migration.Version(), migration.Name(), rollbackErr)
			}
			if recordErr := validateBackendRecord(migration, record); recordErr != nil {
				return recordErr
			}

			result.records = append(result.records, record)
			runner.observe(ctx, Event{
				operation: OperationRollback,
				phase:     PhaseCompleted,
				version:   migration.Version(),
				duration:  elapsed(started),
			})
		}

		return nil
	})

	return result, err
}

type baselineSession interface {
	Session
	Baseline(context.Context, Baseline) (Record, error)
}

// Baseline validates and records an existing schema without replaying any
// historical framework migrations. The owned ledger must be empty, and every
// Go-owned source migration must have a version strictly above the baseline.
// The session must implement Baseline(context.Context, Baseline) (Record, error),
// otherwise ErrBaselineUnsupported is returned. PostgreSQL compares its selected
// catalog fingerprint transactionally; matching it does not verify every schema
// or security property. Cleanup errors can follow a committed baseline.
func (runner *Runner) Baseline(ctx context.Context, baseline Baseline) (record Record, err error) {
	if runner == nil || runner.source == nil || runner.backend == nil {
		return Record{}, ErrInvalidRunner
	}
	if baseline.Version() == 0 || baseline.Name() == "" || baseline.Fingerprint() == (Checksum{}) {
		return Record{}, ErrInvalidBaseline
	}

	available, err := runner.source.Load(ctx)
	if err != nil {
		return Record{}, err
	}
	if err := validateAvailableOrder(available); err != nil {
		return Record{}, err
	}
	for _, migration := range available {
		if migration.Version() <= baseline.Version() {
			return Record{}, fmt.Errorf(
				"%w: migration %d_%s",
				ErrBaselineVersionConflict,
				migration.Version(),
				migration.Name(),
			)
		}
	}
	err = runner.withSession(ctx, func(session Session) error {
		records, recordsErr := session.Records(ctx)
		if recordsErr != nil {
			return fmt.Errorf("read migration records: %w", recordsErr)
		}
		if len(records) != 0 {
			return ErrBaselineExists
		}

		capable, ok := session.(baselineSession)
		if !ok {
			return ErrBaselineUnsupported
		}
		started := time.Now()
		runner.observe(ctx, Event{
			operation: OperationBaseline,
			phase:     PhaseStarted,
			version:   baseline.Version(),
		})
		var baselineErr error
		record, baselineErr = capable.Baseline(ctx, baseline)
		if baselineErr != nil {
			runner.observe(ctx, Event{
				operation: OperationBaseline,
				phase:     PhaseFailed,
				version:   baseline.Version(),
				duration:  elapsed(started),
				err:       observerBaselineError(baselineErr),
			})

			return fmt.Errorf("record schema baseline: %w", baselineErr)
		}
		if record.Kind() != RecordKindBaseline ||
			record.Version() != baseline.Version() ||
			record.Name() != baseline.Name() ||
			record.Checksum() != baseline.Fingerprint() ||
			record.AppliedAt().IsZero() || record.Dirty() {
			return fmt.Errorf("%w: baseline version %d", ErrBackendResult, baseline.Version())
		}
		runner.observe(ctx, Event{
			operation: OperationBaseline,
			phase:     PhaseCompleted,
			version:   baseline.Version(),
			duration:  elapsed(started),
		})

		return nil
	})

	return record, err
}

func observerBaselineError(err error) error {
	if errors.Is(err, ErrBaselineMismatch) {
		return ErrBaselineMismatch
	}

	return err
}

func validateBackendRecord(migration Migration, record Record) error {
	if record.Kind() != RecordKindMigration ||
		record.Version() != migration.Version() ||
		record.Name() != migration.Name() ||
		record.Checksum() != migration.Checksum() ||
		record.AppliedAt().IsZero() ||
		record.Dirty() {
		return fmt.Errorf("%w: apply version %d", ErrBackendResult, migration.Version())
	}

	return nil
}

func (runner *Runner) observe(ctx context.Context, event Event) {
	if runner.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	runner.observer.Observe(ctx, event)
}

func elapsed(started time.Time) time.Duration {
	return max(time.Since(started), 0)
}
