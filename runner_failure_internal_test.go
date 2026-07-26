package migrations

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunnerRejectsInvalidOptionsAndReceivers(t *testing.T) {
	t.Parallel()

	backend := &failureBackend{session: &failureSession{}}
	source := failureSource{}
	if _, err := NewRunner(source, backend, nil); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner(nil option) error = %v", err)
	}
	if _, err := NewRunner(source, backend, WithObserver(nil)); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner(nil observer) error = %v", err)
	}
	if _, err := NewRunner(source, backend, WithUnlockTimeout(-1)); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner(invalid timeout) error = %v", err)
	}
	configured, err := NewRunner(source, backend, WithUnlockTimeout(time.Second))
	if err != nil || configured.unlockTimeout != time.Second {
		t.Fatalf("NewRunner(valid timeout) = %#v, %v", configured, err)
	}

	var runner *Runner
	if _, err := runner.Status(context.Background()); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("nil Status() error = %v", err)
	}
	if _, err := runner.Plan(context.Background()); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("nil Plan() error = %v", err)
	}
	if _, err := runner.Up(context.Background()); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("nil Up() error = %v", err)
	}
	if _, err := runner.Down(context.Background(), 1); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("nil Down() error = %v", err)
	}
	if _, err := runner.Baseline(context.Background(), Baseline{}); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("nil Baseline() error = %v", err)
	}
	if _, err := runner.Recover(context.Background(), Recovery{}); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("nil Recover() error = %v", err)
	}
}

func TestRunnerOperationsPropagateLedgerAndPlanFailures(t *testing.T) {
	t.Parallel()

	fault := errors.New("ledger unavailable")
	migration := internalMigration(t, 1, "operation")
	dirty := internalRecord(t, RecordKindMigration, migration, true)
	baseline := Baseline{version: 100, name: "baseline", fingerprint: ChecksumData([]byte("schema"))}
	recovery := Recovery{version: 1, checksum: migration.Checksum(), action: RecoveryMarkApplied}
	tests := []struct {
		name   string
		source failureSource
		run    func(*Runner) error
	}{
		{name: "plan records", source: failureSource{migrations: []Migration{migration}}, run: func(r *Runner) error { _, err := r.Plan(context.Background()); return err }},
		{name: "up records", source: failureSource{migrations: []Migration{migration}}, run: func(r *Runner) error { _, err := r.Up(context.Background()); return err }},
		{name: "down records", source: failureSource{migrations: []Migration{migration}}, run: func(r *Runner) error { _, err := r.Down(context.Background(), 1); return err }},
		{name: "baseline records", source: failureSource{migrations: []Migration{internalMigration(t, 101, "after")}}, run: func(r *Runner) error { _, err := r.Baseline(context.Background(), baseline); return err }},
		{name: "recover records", source: failureSource{migrations: []Migration{migration}}, run: func(r *Runner) error { _, err := r.Recover(context.Background(), recovery); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := NewRunner(test.source, &failureBackend{session: &failureSession{records: []Record{dirty}, recordsErr: fault}})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}
			if err := test.run(runner); !errors.Is(err, fault) {
				t.Fatalf("operation error = %v, want ledger fault", err)
			}
		})
	}

	diverged := &failureSession{records: []Record{dirty}}
	runner, err := NewRunner(failureSource{migrations: []Migration{migration}}, &failureBackend{session: diverged})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Up(context.Background()); !errors.Is(err, ErrDirty) {
		t.Fatalf("Up(plan) error = %v", err)
	}
	if _, err := runner.Down(context.Background(), 0); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("Down(plan) error = %v", err)
	}

	rollbackFault := errors.New("rollback failed")
	clean := internalRecord(t, RecordKindMigration, migration, false)
	runner, err = NewRunner(
		failureSource{migrations: []Migration{migration}},
		&failureBackend{session: &failureSession{records: []Record{clean}, rollbackErr: rollbackFault}},
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Down(context.Background(), 1); !errors.Is(err, rollbackFault) {
		t.Fatalf("Down(rollback) error = %v", err)
	}

	malformedSourceRunner, err := NewRunner(
		failureSource{migrations: []Migration{{}}},
		&failureBackend{session: &failureSession{}},
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := malformedSourceRunner.Baseline(context.Background(), baseline); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("Baseline(source) error = %v", err)
	}
	if _, err := malformedSourceRunner.Recover(context.Background(), recovery); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("Recover(source) error = %v", err)
	}

	malformedLedgerRunner, err := NewRunner(
		failureSource{migrations: []Migration{migration}},
		&failureBackend{session: &failureSession{records: []Record{{}}}},
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := malformedLedgerRunner.Recover(context.Background(), recovery); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Recover(ledger) error = %v", err)
	}
}

func TestRunnerPropagatesSourceFailuresBeforeLocking(t *testing.T) {
	t.Parallel()

	fault := errors.New("source unavailable")
	runner, err := NewRunner(failureSource{err: fault}, &failureBackend{session: &failureSession{}})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	baseline := Baseline{version: 1, name: "baseline", fingerprint: ChecksumData([]byte("schema"))}
	recovery := Recovery{version: 1, checksum: ChecksumData([]byte("migration")), action: RecoveryMarkApplied}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "status", run: func() error { _, runErr := runner.Status(context.Background()); return runErr }},
		{name: "plan", run: func() error { _, runErr := runner.Plan(context.Background()); return runErr }},
		{name: "up", run: func() error { _, runErr := runner.Up(context.Background()); return runErr }},
		{name: "down", run: func() error { _, runErr := runner.Down(context.Background(), 1); return runErr }},
		{name: "baseline", run: func() error { _, runErr := runner.Baseline(context.Background(), baseline); return runErr }},
		{name: "recover", run: func() error { _, runErr := runner.Recover(context.Background(), recovery); return runErr }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, fault) {
				t.Fatalf("operation error = %v, want source fault", err)
			}
		})
	}
}

func TestLockedRunnerFailureBoundariesAlwaysRelease(t *testing.T) {
	t.Parallel()

	fault := errors.New("injected runner fault")
	tests := []struct {
		name    string
		backend *failureBackend
		target  error
	}{
		{name: "acquire", backend: &failureBackend{acquireErr: fault}, target: fault},
		{name: "prepare", backend: &failureBackend{session: &failureSession{prepareErr: fault}}, target: fault},
		{name: "records", backend: &failureBackend{session: &failureSession{recordsErr: fault}}, target: fault},
		{name: "release", backend: &failureBackend{session: &failureSession{releaseErr: fault}}, target: fault},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := NewRunner(failureSource{}, test.backend)
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}
			_, err = runner.Status(context.Background())
			if !errors.Is(err, test.target) {
				t.Fatalf("Status() error = %v, want %v", err, test.target)
			}
			if test.name != "acquire" && !test.backend.session.(*failureSession).released {
				t.Fatal("session was not released")
			}
		})
	}
}

func TestRunnerRejectsBackendContractViolations(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 1, "contract")
	invalid := Record{}
	tests := []struct {
		name string
		run  func(*Runner) error
	}{
		{
			name: "apply record",
			run: func(runner *Runner) error {
				_, err := runner.Up(context.Background())
				return err
			},
		},
		{
			name: "rollback record",
			run: func(runner *Runner) error {
				_, err := runner.Down(context.Background(), 1)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &failureSession{applyRecord: invalid, rollbackRecord: invalid}
			if test.name == "rollback record" {
				session.records = []Record{internalRecord(t, RecordKindMigration, migration, false)}
			}
			runner, err := NewRunner(failureSource{migrations: []Migration{migration}}, &failureBackend{session: session})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}
			if err := test.run(runner); !errors.Is(err, ErrBackendResult) {
				t.Fatalf("runner error = %v, want ErrBackendResult", err)
			}
		})
	}
}

func TestEventAccessorsAndObserverPanicIsolation(t *testing.T) {
	t.Parallel()

	fault := errors.New("event failure")
	event := Event{
		operation: OperationApply,
		phase:     PhaseFailed,
		version:   42,
		duration:  time.Second,
		err:       fault,
	}
	if event.Operation() != OperationApply || event.Phase() != PhaseFailed ||
		event.Version() != 42 || event.Duration() != time.Second || !errors.Is(event.Err(), fault) {
		t.Fatalf("event accessors returned inconsistent values: %#v", event)
	}

	runner := &Runner{observer: panicObserver{}}
	runner.observe(context.Background(), event)
	(&Runner{}).observe(context.Background(), event)
	if elapsed(time.Now().Add(time.Hour)) != 0 {
		t.Fatal("elapsed(future) did not clamp to zero")
	}
}

func TestBaselineCapabilityFailures(t *testing.T) {
	t.Parallel()

	fingerprint := ChecksumData([]byte("reviewed schema"))
	baseline := Baseline{version: 100, name: "reviewed_schema", fingerprint: fingerprint}
	source := failureSource{migrations: []Migration{internalMigration(t, 101, "next")}}

	runner, err := NewRunner(source, &failureBackend{session: &failureSession{}})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Baseline(context.Background(), baseline); !errors.Is(err, ErrBaselineUnsupported) {
		t.Fatalf("Baseline(unsupported) error = %v", err)
	}

	fault := errors.New("baseline persistence failed")
	capable := &capableFailureSession{failureSession: &failureSession{}, baselineErr: fault}
	runner, err = NewRunner(source, &failureBackend{session: capable})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Baseline(context.Background(), baseline); !errors.Is(err, fault) {
		t.Fatalf("Baseline(fault) error = %v", err)
	}

	capable.baselineErr = nil
	capable.baselineRecord = Record{}
	if _, err := runner.Baseline(context.Background(), baseline); !errors.Is(err, ErrBackendResult) {
		t.Fatalf("Baseline(record) error = %v", err)
	}

	if _, err := runner.Baseline(context.Background(), Baseline{}); !errors.Is(err, ErrInvalidBaseline) {
		t.Fatalf("Baseline(invalid) error = %v", err)
	}
}

func TestRecoveryCapabilityFailures(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 1, "recoverable")
	dirty := internalRecord(t, RecordKindMigration, migration, true)
	recovery := Recovery{version: migration.Version(), checksum: migration.Checksum(), action: RecoveryMarkApplied}
	source := failureSource{migrations: []Migration{migration}}

	runner, err := NewRunner(source, &failureBackend{session: &failureSession{records: []Record{dirty}}})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Recover(context.Background(), recovery); !errors.Is(err, ErrRecoveryUnsupported) {
		t.Fatalf("Recover(unsupported) error = %v", err)
	}

	fault := errors.New("recovery persistence failed")
	capable := &capableFailureSession{
		failureSession: &failureSession{records: []Record{dirty}},
		recoveryErr:    fault,
	}
	runner, err = NewRunner(source, &failureBackend{session: capable})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Recover(context.Background(), recovery); !errors.Is(err, fault) {
		t.Fatalf("Recover(fault) error = %v", err)
	}

	capable.recoveryErr = nil
	capable.recoveryRecord = Record{}
	if _, err := runner.Recover(context.Background(), recovery); !errors.Is(err, ErrBackendResult) {
		t.Fatalf("Recover(record) error = %v", err)
	}

	second := internalMigration(t, 2, "also_dirty")
	conflictSession := &failureSession{records: []Record{
		dirty,
		internalRecord(t, RecordKindMigration, second, true),
	}}
	runner, err = NewRunner(
		failureSource{migrations: []Migration{migration, second}},
		&failureBackend{session: conflictSession},
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Recover(context.Background(), recovery); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("Recover(conflict) error = %v", err)
	}

	if _, err := runner.Recover(context.Background(), Recovery{}); !errors.Is(err, ErrInvalidRecovery) {
		t.Fatalf("Recover(invalid) error = %v", err)
	}
}

type failureSource struct {
	migrations []Migration
	err        error
}

func (source failureSource) Load(context.Context) ([]Migration, error) {
	return append([]Migration(nil), source.migrations...), source.err
}

type failureBackend struct {
	acquireErr error
	session    Session
}

func (backend *failureBackend) Acquire(context.Context) (Session, error) {
	return backend.session, backend.acquireErr
}

type failureSession struct {
	prepareErr     error
	records        []Record
	recordsErr     error
	applyRecord    Record
	applyErr       error
	rollbackRecord Record
	rollbackErr    error
	releaseErr     error
	released       bool
}

func (session *failureSession) Prepare(context.Context) error { return session.prepareErr }

type capableFailureSession struct {
	*failureSession
	baselineRecord Record
	baselineErr    error
	recoveryRecord Record
	recoveryErr    error
}

func (session *capableFailureSession) Baseline(context.Context, Baseline) (Record, error) {
	return session.baselineRecord, session.baselineErr
}

func (session *capableFailureSession) Recover(
	context.Context,
	Migration,
	RecoveryAction,
) (Record, error) {
	return session.recoveryRecord, session.recoveryErr
}

func (session *failureSession) Records(context.Context) ([]Record, error) {
	return append([]Record(nil), session.records...), session.recordsErr
}

func (session *failureSession) Apply(context.Context, Migration) (Record, error) {
	return session.applyRecord, session.applyErr
}

func (session *failureSession) Rollback(context.Context, Migration) (Record, error) {
	return session.rollbackRecord, session.rollbackErr
}

func (session *failureSession) Release(context.Context) error {
	session.released = true

	return session.releaseErr
}

type panicObserver struct{}

func (panicObserver) Observe(context.Context, Event) { panic("observer failure") }
