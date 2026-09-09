package postgres

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestBackendOptionsAreAppliedLeftToRightAndLastDuplicateWins(t *testing.T) {
	events := make([]string, 0, 3)
	option := func(name string, apply func(*Backend)) Option {
		return func(backend *Backend) error {
			events = append(events, name)
			apply(backend)
			return nil
		}
	}
	database := &sql.DB{}
	backend, err := New(database,
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
	if backend.database != database {
		t.Fatal("New() replaced the caller-owned database")
	}
}

func TestBackendOptionsRejectNilAndStopAtFirstErrorWithoutDatabaseWork(t *testing.T) {
	database := &sql.DB{}
	if _, err := New(database, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(nil option) error = %v", err)
	}
	want := errors.New("option failed")
	events := make([]string, 0, 2)
	_, err := New(database,
		func(*Backend) error { events = append(events, "first"); return want },
		func(*Backend) error { events = append(events, "later"); return nil },
	)
	if !errors.Is(err, want) || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() error = %v", err)
	}
	if !sameStrings(events, []string{"first"}) {
		t.Fatalf("option order = %v", events)
	}
	// An unopened zero-value sql.DB would panic if construction tried to acquire
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
