package migrationsservice_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	migrations "github.com/faustbrian/go-migrations"
	migrationsservice "github.com/faustbrian/go-migrations/adapters/service"
	service "github.com/faustbrian/go-service"
)

func TestCommandPreservesOneShotOrderingAndCallerOwnership(t *testing.T) {
	runner := testRunner(t)
	events := make([]string, 0, 5)
	adapter, err := migrationsservice.New(migrationsservice.Options[string]{
		Summary: "run database migrations",
		Load: func(_ context.Context, invocation service.Invocation) (string, error) {
			events = append(events, "load")
			return invocation.Environment[0], nil
		},
		Prepare: func(_ context.Context, build service.BuildContext, configuration string) (migrationsservice.Execution, error) {
			if build.Identity.Name != "postal" || build.Identity.Role != "migrate" {
				t.Fatalf("process identity = %#v", build.Identity)
			}
			events = append(events, "prepare "+configuration)
			return migrationsservice.Execution{
				Runner: runner,
				Components: []service.Component{{
					Name:  "database",
					Start: func(context.Context) error { events = append(events, "start"); return nil },
					Stop:  func(context.Context) error { events = append(events, "stop"); return nil },
				}},
			}, nil
		},
		Execute: func(_ context.Context, actual *migrations.Runner) error {
			if actual != runner {
				t.Fatal("Execute() received a different runner")
			}
			events = append(events, "execute")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var stderr bytes.Buffer
	exitCode := service.Execute(context.Background(), service.Definition{
		Identity: service.Identity{Name: "postal"},
		Commands: service.Commands{Migrate: adapter.Command()},
	}, service.Invocation{
		Args: []string{"migrate"}, Environment: []string{"DATABASE_URL=configured"},
		Stdout: io.Discard, Stderr: &stderr,
	})
	if exitCode != 0 {
		t.Fatalf("Execute() exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	want := []string{"load", "prepare DATABASE_URL=configured", "start", "execute", "stop"}
	if !equalEvents(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestNewAndCommandPreserveStableErrorTraversal(t *testing.T) {
	validLoad := func(context.Context, service.Invocation) (struct{}, error) { return struct{}{}, nil }
	validPrepare := func(context.Context, service.BuildContext, struct{}) (migrationsservice.Execution, error) {
		return migrationsservice.Execution{}, nil
	}
	validExecute := func(context.Context, *migrations.Runner) error { return nil }
	for _, options := range []migrationsservice.Options[struct{}]{
		{Load: validLoad, Prepare: validPrepare, Execute: validExecute},
		{Summary: "migrate", Prepare: validPrepare, Execute: validExecute},
		{Summary: "migrate", Load: validLoad, Execute: validExecute},
		{Summary: "migrate", Load: validLoad, Prepare: validPrepare},
	} {
		_, err := migrationsservice.New(options)
		if !errors.Is(err, migrationsservice.ErrInvalidOptions) {
			t.Fatalf("New() error = %v, want ErrInvalidOptions", err)
		}
		var optionsError *migrationsservice.OptionsError
		if !errors.As(err, &optionsError) || optionsError.Error() == "" {
			t.Fatalf("New() error = %v, want OptionsError", err)
		}
	}

	adapter, err := migrationsservice.New(migrationsservice.Options[struct{}]{
		Summary: "migrate", Load: validLoad, Prepare: validPrepare, Execute: validExecute,
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := service.Execute(context.Background(), service.Definition{
		Identity: service.Identity{Name: "postal"}, Commands: service.Commands{Migrate: adapter.Command()},
	}, service.Invocation{Args: []string{"migrate"}, Stdout: io.Discard, Stderr: &stderr}); code == 0 {
		t.Fatal("Execute() succeeded without a runner")
	}
	executionErr := &migrationsservice.ExecutionError{Field: "Runner", Reason: "must not be nil"}
	if !errors.Is(executionErr, migrationsservice.ErrInvalidExecution) || executionErr.Error() == "" {
		t.Fatalf("ExecutionError = %v", executionErr)
	}
}

func TestCommandPreservesPreparationFailure(t *testing.T) {
	want := errors.New("prepare failed")
	adapter, err := migrationsservice.New(migrationsservice.Options[struct{}]{
		Summary: "migrate",
		Load:    func(context.Context, service.Invocation) (struct{}, error) { return struct{}{}, nil },
		Prepare: func(context.Context, service.BuildContext, struct{}) (migrationsservice.Execution, error) {
			return migrationsservice.Execution{}, want
		},
		Execute: func(context.Context, *migrations.Runner) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := service.Execute(context.Background(), service.Definition{
		Identity: service.Identity{Name: "postal"}, Commands: service.Commands{Migrate: adapter.Command()},
	}, service.Invocation{Args: []string{"migrate"}, Stdout: io.Discard, Stderr: &stderr}); code == 0 {
		t.Fatal("Execute() succeeded after preparation failure")
	}
}

func TestCommandStopsComponentsAfterExecutionFailure(t *testing.T) {
	want := errors.New("execute failed")
	events := make([]string, 0, 3)
	adapter, err := migrationsservice.New(migrationsservice.Options[struct{}]{
		Summary: "migrate",
		Load:    func(context.Context, service.Invocation) (struct{}, error) { return struct{}{}, nil },
		Prepare: func(context.Context, service.BuildContext, struct{}) (migrationsservice.Execution, error) {
			return migrationsservice.Execution{
				Runner: testRunner(t),
				Components: []service.Component{{
					Name:  "database",
					Start: func(context.Context) error { events = append(events, "start"); return nil },
					Stop:  func(context.Context) error { events = append(events, "stop"); return nil },
				}},
			}, nil
		},
		Execute: func(context.Context, *migrations.Runner) error {
			events = append(events, "execute")
			return want
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := service.Execute(context.Background(), service.Definition{
		Identity: service.Identity{Name: "postal"}, Commands: service.Commands{Migrate: adapter.Command()},
	}, service.Invocation{Args: []string{"migrate"}, Stdout: io.Discard, Stderr: &stderr}); code == 0 {
		t.Fatal("Execute() succeeded after migration failure")
	}
	if !equalEvents(events, []string{"start", "execute", "stop"}) {
		t.Fatalf("events = %v", events)
	}
}

type source struct{}

func (source) Load(context.Context) ([]migrations.Migration, error) { return nil, nil }

type backend struct{}

func (backend) Acquire(context.Context) (migrations.Session, error) {
	return nil, errors.New("not used")
}

func testRunner(t *testing.T) *migrations.Runner {
	t.Helper()
	runner, err := migrations.NewRunner(source{}, backend{})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	return runner
}

func equalEvents(left, right []string) bool {
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
