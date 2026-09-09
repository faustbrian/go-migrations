package migrations

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunnerOptionsAreAppliedLeftToRightAndLastDuplicateWins(t *testing.T) {
	events := make([]string, 0, 3)
	firstObserver := &optionObserver{}
	lastObserver := &optionObserver{}
	option := func(name string, apply func(*Runner)) Option {
		return func(runner *Runner) error {
			events = append(events, name)
			apply(runner)
			return nil
		}
	}
	backend := &countingBackend{}
	runner, err := NewRunner(failureSource{}, backend,
		option("first", func(runner *Runner) { runner.observer = firstObserver }),
		option("second", func(runner *Runner) { runner.unlockTimeout = time.Second }),
		option("third", func(runner *Runner) { runner.observer = lastObserver }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if !sameOptionStrings(events, []string{"first", "second", "third"}) {
		t.Fatalf("option order = %v", events)
	}
	if runner.observer != lastObserver || runner.unlockTimeout != time.Second {
		t.Fatalf("duplicate precedence = %#v", runner)
	}
	if backend.acquireCalls != 0 {
		t.Fatalf("Acquire() calls = %d", backend.acquireCalls)
	}
}

func TestRunnerOptionsStopAtFirstErrorWithoutAcquiringBackend(t *testing.T) {
	want := errors.New("option failed")
	events := make([]string, 0, 2)
	backend := &countingBackend{}
	_, err := NewRunner(failureSource{}, backend,
		func(*Runner) error { events = append(events, "first"); return want },
		func(*Runner) error { events = append(events, "later"); return nil },
	)
	if !errors.Is(err, want) || !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if !sameOptionStrings(events, []string{"first"}) {
		t.Fatalf("option order = %v", events)
	}
	if backend.acquireCalls != 0 {
		t.Fatalf("Acquire() calls = %d", backend.acquireCalls)
	}
}

type countingBackend struct{ acquireCalls int }

func (backend *countingBackend) Acquire(context.Context) (Session, error) {
	backend.acquireCalls++
	return nil, errors.New("unexpected acquire")
}

type optionObserver struct{}

func (*optionObserver) Observe(context.Context, Event) {}

func sameOptionStrings(left, right []string) bool {
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
