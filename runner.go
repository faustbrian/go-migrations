package migrations

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const defaultUnlockTimeout = 30 * time.Second

var (
	// ErrInvalidRunner indicates missing or inconsistent runner dependencies.
	ErrInvalidRunner = errors.New("invalid migration runner")
	// ErrBackendResult indicates an engine violated the public backend contract.
	ErrBackendResult = errors.New("invalid migration backend result")
)

// Session represents exclusive migration-job ownership bound to one physical
// database connection. Ledger preparation, reads, and execution all happen
// through this value so connection loss and lock loss cannot be separated from
// execution and a one-connection pool cannot deadlock during preparation.
type Session interface {
	Prepare(context.Context) error
	Records(context.Context) ([]Record, error)
	Apply(context.Context, Migration) (Record, error)
	Rollback(context.Context, Migration) (Record, error)
	Release(context.Context) error
}

// Backend is the replaceable migration-engine conformance boundary.
//
// Session.Apply must return only after both execution and owned-ledger
// persistence have reached an explicit recoverable outcome. Implementations
// must leave a dirty record when non-transactional execution has an uncertain
// or partial result.
type Backend interface {
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
// adapters. SQL text is deliberately excluded.
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

// Err returns the operation failure without exposing SQL contents.
func (event Event) Err() error { return event.err }

// Observer consumes structured migration events.
type Observer interface {
	Observe(context.Context, Event)
}

// Result describes the migrations completed by one runner invocation.
type Result struct {
	records []Record
}

// Records returns a copy of completed ledger records.
func (result Result) Records() []Record {
	return append([]Record(nil), result.records...)
}

// Option configures a Runner.
type Option func(*Runner) error

// WithObserver installs a structured event observer. Observer panics are
// contained so diagnostics cannot change migration outcomes.
func WithObserver(observer Observer) Option {
	return func(runner *Runner) error {
		if observer == nil {
			return ErrInvalidRunner
		}
		runner.observer = observer

		return nil
	}
}

// WithUnlockTimeout bounds best-effort lock release after the job context is
// canceled. Release always uses a detached context so cancellation cannot skip
// cleanup.
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

// NewRunner constructs an engine-neutral migration runner.
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

// Status returns a locked, internally consistent source and ledger snapshot.
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

// Plan returns a locked dry-run plan without executing migration SQL.
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
// verified that its effects are either fully applied or fully removed.
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
	var migration Migration
	for _, candidate := range available {
		if candidate.Version() == recovery.Version() {
			migration = candidate
			break
		}
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
			if entry.Version() == recovery.Version() && entry.Checksum() == recovery.Checksum() {
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

// Up applies all pending migrations. Source and ledger history are revalidated
// while holding the lock, eliminating plan-to-execution races.
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

// Down rolls back exactly count applied migrations, newest first. The source
// and complete ledger history are revalidated while holding the lock.
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
				err:       baselineErr,
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
	duration := time.Since(started)
	if duration < 0 {
		return 0
	}

	return duration
}
