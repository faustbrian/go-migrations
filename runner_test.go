package migrations_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func TestRunnerUpLocksRevalidatesAndAppliesPlan(t *testing.T) {
	t.Parallel()

	first := mustMigration(t, 1, "create_users", "CREATE TABLE users (id bigint);\n")
	second := mustMigration(t, 2, "add_email", "ALTER TABLE users ADD email text;\n")
	backend := &recordingBackend{}
	observer := &recordingObserver{}
	runner, err := migrations.NewRunner(
		staticSource{migrations: []migrations.Migration{first, second}},
		backend,
		migrations.WithObserver(observer),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Up(context.Background())
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if len(result.Records()) != 2 {
		t.Fatalf("Records() count = %d, want 2", len(result.Records()))
	}
	if got := backend.calls(); !equalStrings(got, []string{"acquire", "prepare", "records", "apply:1", "apply:2", "release"}) {
		t.Fatalf("backend calls = %v", got)
	}
	if got := observer.operations(); !equalOperations(got, []migrations.Operation{
		migrations.OperationLock,
		migrations.OperationApply,
		migrations.OperationApply,
		migrations.OperationUnlock,
	}) {
		t.Fatalf("observer operations = %v", got)
	}

	records := result.Records()
	records[0] = migrations.Record{}
	if result.Records()[0].Version() != 1 {
		t.Fatal("Records() exposed mutable result storage")
	}
}

func TestRunnerUpReturnsPartialResultAndStillUnlocks(t *testing.T) {
	t.Parallel()

	first := mustMigration(t, 1, "create_users", "CREATE TABLE users (id bigint);\n")
	second := mustMigration(t, 2, "add_email", "ALTER TABLE users ADD email text;\n")
	applyError := errors.New("connection lost")
	backend := &recordingBackend{failVersion: 2, applyError: applyError}
	runner, err := migrations.NewRunner(
		staticSource{migrations: []migrations.Migration{first, second}},
		backend,
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Up(context.Background())
	if !errors.Is(err, applyError) {
		t.Fatalf("Up() error = %v, want apply error", err)
	}
	if len(result.Records()) != 1 || result.Records()[0].Version() != 1 {
		t.Fatalf("Records() = %#v, want completed version 1", result.Records())
	}
	if got := backend.calls(); got[len(got)-1] != "release" {
		t.Fatalf("last backend call = %q, want release", got[len(got)-1])
	}
}

func TestRunnerUpReportsUnlockFailureWithoutHidingApplyFailure(t *testing.T) {
	t.Parallel()

	migration := mustMigration(t, 1, "create_users", "SELECT 1;\n")
	applyError := errors.New("apply failed")
	unlockError := errors.New("unlock failed")
	backend := &recordingBackend{
		failVersion: 1,
		applyError:  applyError,
		lock:        &recordingLock{releaseError: unlockError},
	}
	runner, err := migrations.NewRunner(staticSource{migrations: []migrations.Migration{migration}}, backend)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Up(context.Background())
	if !errors.Is(err, applyError) || !errors.Is(err, unlockError) {
		t.Fatalf("Up() error = %v, want joined apply and unlock errors", err)
	}
}

func TestRunnerDownRollsBackNewestMigrationUnderLock(t *testing.T) {
	t.Parallel()

	first := mustReversibleMigration(t, 1, "create_users")
	second := mustReversibleMigration(t, 2, "add_email")
	backend := &recordingBackend{records: []migrations.Record{
		mustRecord(t, migrations.RecordKindMigration, first, false),
		mustRecord(t, migrations.RecordKindMigration, second, false),
	}}
	runner, err := migrations.NewRunner(
		staticSource{migrations: []migrations.Migration{first, second}},
		backend,
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Down(context.Background(), 1)
	if err != nil {
		t.Fatalf("Down() error = %v", err)
	}
	if len(result.Records()) != 1 || result.Records()[0].Version() != 2 {
		t.Fatalf("Records() = %#v, want rolled-back version 2", result.Records())
	}
	if got := backend.calls(); !equalStrings(got, []string{
		"acquire",
		"prepare",
		"records",
		"rollback:2",
		"release",
	}) {
		t.Fatalf("backend calls = %v", got)
	}
}

func TestRunnerRejectsInvalidDependencies(t *testing.T) {
	t.Parallel()

	backend := &recordingBackend{}
	if _, err := migrations.NewRunner(nil, backend); !errors.Is(err, migrations.ErrInvalidRunner) {
		t.Fatalf("NewRunner(nil source) error = %v", err)
	}
	if _, err := migrations.NewRunner(staticSource{}, nil); !errors.Is(err, migrations.ErrInvalidRunner) {
		t.Fatalf("NewRunner(nil backend) error = %v", err)
	}
	if _, err := migrations.NewRunner(
		staticSource{},
		backend,
		migrations.WithUnlockTimeout(0),
	); !errors.Is(err, migrations.ErrInvalidRunner) {
		t.Fatalf("NewRunner(unlock timeout) error = %v", err)
	}
}

type staticSource struct {
	migrations []migrations.Migration
	err        error
}

func (source staticSource) Load(context.Context) ([]migrations.Migration, error) {
	return append([]migrations.Migration(nil), source.migrations...), source.err
}

type recordingBackend struct {
	mu                  sync.Mutex
	log                 []string
	records             []migrations.Record
	failVersion         migrations.Version
	applyError          error
	lock                *recordingLock
	baselineFingerprint migrations.Checksum
}

func (backend *recordingBackend) Acquire(context.Context) (migrations.Session, error) {
	backend.append("acquire")
	if backend.lock == nil {
		backend.lock = &recordingLock{onRelease: func() { backend.append("release") }}
	} else {
		backend.lock.onRelease = func() { backend.append("release") }
	}

	return &recordingSession{backend: backend, lock: backend.lock}, nil
}

type recordingSession struct {
	backend *recordingBackend
	lock    *recordingLock
}

func (session *recordingSession) Prepare(context.Context) error {
	session.backend.append("prepare")

	return nil
}

func (session *recordingSession) Records(context.Context) ([]migrations.Record, error) {
	session.backend.append("records")

	return append([]migrations.Record(nil), session.backend.records...), nil
}

func (session *recordingSession) Apply(_ context.Context, migration migrations.Migration) (migrations.Record, error) {
	session.backend.append("apply:" + migration.Version().String())
	if migration.Version() == session.backend.failVersion {
		return migrations.Record{}, session.backend.applyError
	}

	record, err := migrations.NewRecord(
		migrations.RecordKindMigration,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		time.Unix(1_700_000_000, 0).UTC(),
		time.Millisecond,
		false,
	)
	if err != nil {
		return migrations.Record{}, err
	}
	session.backend.records = append(session.backend.records, record)

	return record, nil
}

func (session *recordingSession) Rollback(_ context.Context, migration migrations.Migration) (migrations.Record, error) {
	session.backend.append("rollback:" + migration.Version().String())
	for index, record := range session.backend.records {
		if record.Version() == migration.Version() {
			session.backend.records = append(
				session.backend.records[:index],
				session.backend.records[index+1:]...,
			)

			return record, nil
		}
	}

	return migrations.Record{}, migrations.ErrDeletedMigration
}

func (session *recordingSession) Baseline(_ context.Context, baseline migrations.Baseline) (migrations.Record, error) {
	session.backend.append("baseline:" + baseline.Version().String())
	if baseline.Fingerprint() != session.backend.baselineFingerprint {
		return migrations.Record{}, migrations.ErrBaselineMismatch
	}

	record, err := migrations.NewRecord(
		migrations.RecordKindBaseline,
		baseline.Version(),
		baseline.Name(),
		baseline.Fingerprint(),
		time.Unix(1_700_000_000, 0).UTC(),
		0,
		false,
	)
	if err != nil {
		return migrations.Record{}, err
	}
	session.backend.records = append(session.backend.records, record)

	return record, nil
}

func (session *recordingSession) Recover(
	_ context.Context,
	migration migrations.Migration,
	action migrations.RecoveryAction,
) (migrations.Record, error) {
	actionName := "rolled-back"
	if action == migrations.RecoveryMarkApplied {
		actionName = "applied"
	}
	session.backend.append("recover:" + migration.Version().String() + ":" + actionName)
	for index, record := range session.backend.records {
		if record.Version() != migration.Version() || !record.Dirty() {
			continue
		}
		if action == migrations.RecoveryMarkRolledBack {
			session.backend.records = append(
				session.backend.records[:index],
				session.backend.records[index+1:]...,
			)

			return record, nil
		}
		clean, err := migrations.NewRecord(
			record.Kind(),
			record.Version(),
			record.Name(),
			record.Checksum(),
			record.AppliedAt(),
			record.Duration(),
			false,
		)
		if err != nil {
			return migrations.Record{}, err
		}
		session.backend.records[index] = clean

		return clean, nil
	}

	return migrations.Record{}, migrations.ErrNoDirtyMigration
}

func (session *recordingSession) Release(ctx context.Context) error {
	return session.lock.Release(ctx)
}

func (backend *recordingBackend) append(call string) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.log = append(backend.log, call)
}

func (backend *recordingBackend) calls() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()

	return append([]string(nil), backend.log...)
}

func (backend *recordingBackend) clearCalls() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.log = nil
}

type recordingLock struct {
	onRelease    func()
	releaseError error
}

func (lock *recordingLock) Release(context.Context) error {
	if lock.onRelease != nil {
		lock.onRelease()
	}

	return lock.releaseError
}

type recordingObserver struct {
	mu     sync.Mutex
	events []migrations.Event
}

func (observer *recordingObserver) Observe(_ context.Context, event migrations.Event) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.events = append(observer.events, event)
}

func (observer *recordingObserver) operations() []migrations.Operation {
	observer.mu.Lock()
	defer observer.mu.Unlock()

	result := make([]migrations.Operation, 0, len(observer.events))
	for _, event := range observer.events {
		if event.Phase() == migrations.PhaseCompleted {
			result = append(result, event.Operation())
		}
	}

	return result
}

func equalStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}

func equalOperations(left []migrations.Operation, right []migrations.Operation) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}
