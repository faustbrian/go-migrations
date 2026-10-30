package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBackendDefaultLockTimeoutBoundsConnectionAcquisition(t *testing.T) {
	t.Parallel()

	deadlines := make(chan time.Time, 1)
	mock := NewMockConn()
	mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(NewMockRows([]string{"acquired"}).AddRow(true))
	mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(NewMockRows([]string{"unlocked"}).AddRow(true))
	database := &MockPool{Conn: mock, OnAcquire: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("connection acquisition context has no deadline")
		}
		deadlines <- deadline
		return nil
	}}
	backend, err := newBackend(database)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startedAt := time.Now()
	session, err := backend.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := session.Release(context.Background()); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	deadline := <-deadlines
	budget := deadline.Sub(startedAt)
	if budget < 29*time.Second || budget > 31*time.Second {
		t.Fatalf("default acquisition deadline budget = %v, want approximately 30s", budget)
	}
}

func TestOperationGateHonorsCancellationWhileAnotherOperationOwnsSession(t *testing.T) {
	t.Parallel()

	gate := newOperationGate()
	release, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire error = %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("contended acquire error = %v, want context canceled", err)
	}
}

func TestBeginOperationHonorsCancellationWhileSessionIsBusy(t *testing.T) {
	t.Parallel()

	gate := newOperationGate()
	release, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire error = %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &session{gate: gate, operationTimeout: time.Hour}
	if _, _, err := session.beginOperation(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("beginOperation() error = %v, want context cancellation", err)
	}
}

func TestAggregateTextBudgetAcceptsExactLimitAndRejectsNextByte(t *testing.T) {
	t.Parallel()

	half := strings.Repeat("x", MaxLedgerBytes/2)
	total, err := boundedTextBytes(0, MaxLedgerBytes, half, half)
	if err != nil || total != MaxLedgerBytes {
		t.Fatalf("exact aggregate budget = %d, %v", total, err)
	}
	if _, err := boundedTextBytes(total, MaxLedgerBytes, "x"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("next aggregate byte error = %v, want ErrResourceLimit", err)
	}
}

func TestBackendOptionsAreAppliedLeftToRightAndLastDuplicateWins(t *testing.T) {
	events := make([]string, 0, 3)
	option := func(name string, apply func(*Backend)) Option {
		return func(backend *Backend) error {
			events = append(events, name)
			apply(backend)
			return nil
		}
	}
	database := &MockPool{}
	backend, err := newBackend(database,
		option("first", func(backend *Backend) { backend.lockTimeout = time.Second }),
		option("second", func(backend *Backend) { backend.statementTimeout = 2 * time.Second }),
		option("third", func(backend *Backend) { backend.lockTimeout = 3 * time.Second }),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !sameStrings(events, []string{"first", "second", "third"}) {
		t.Fatalf("option order = %v", events)
	}
	if backend.lockTimeout != 3*time.Second || backend.statementTimeout != 2*time.Second {
		t.Fatalf("duplicate precedence = %#v", backend)
	}
	if backend.pool != database {
		t.Fatal("New() replaced the caller-owned database")
	}
}

func TestBackendOptionsRejectNilAndStopAtFirstErrorWithoutDatabaseWork(t *testing.T) {
	database := &MockPool{}
	if _, err := newBackend(database, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(nil option) error = %v", err)
	}
	want := errors.New("option failed")
	events := make([]string, 0, 2)
	_, err := newBackend(database,
		func(*Backend) error { events = append(events, "first"); return want },
		func(*Backend) error { events = append(events, "later"); return nil },
	)
	if !errors.Is(err, want) || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() error = %v", err)
	}
	if !sameStrings(events, []string{"first"}) {
		t.Fatalf("option order = %v", events)
	}
	// A MockPool without a script would panic if construction tried to acquire
	// a connection. Reaching this assertion characterizes New as I/O-free.
}

func sameStrings(left, right []string) bool {
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
