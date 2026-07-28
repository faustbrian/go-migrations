package migrationsservice_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/faustbrian/golib/pkg/migrations"
	"github.com/faustbrian/golib/pkg/migrations/migrationsservice"
	"github.com/faustbrian/golib/pkg/service"
)

func TestCommandRunsCallerSelectedMigrationAsOneShotWork(t *testing.T) {
	runner := testRunner(t)
	events := make([]string, 0, 5)
	adapter, err := migrationsservice.New(migrationsservice.Options[string]{
		Summary: "run database migrations",
		Load: func(_ context.Context, invocation service.Invocation) (string, error) {
			events = append(events, "load")

			return invocation.Environment[0], nil
		},
		Prepare: func(
			_ context.Context,
			build service.BuildContext,
			configuration string,
		) (migrationsservice.Execution, error) {
			if build.Identity.Name != "postal" ||
				build.Identity.Role != "migrate" {
				t.Fatalf("process identity = %#v", build.Identity)
			}
			events = append(events, "prepare "+configuration)

			return migrationsservice.Execution{
				Runner: runner,
				Components: []service.Component{{
					Name: "database",
					Start: func(context.Context) error {
						events = append(events, "start")

						return nil
					},
					Stop: func(context.Context) error {
						events = append(events, "stop")

						return nil
					},
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
	exitCode := service.Execute(
		context.Background(),
		service.Definition{
			Identity: service.Identity{Name: "postal"},
			Commands: service.Commands{Migrate: adapter.Command()},
		},
		service.Invocation{
			Args:        []string{"migrate"},
			Environment: []string{"DATABASE_URL=configured"},
			Stdout:      io.Discard,
			Stderr:      &stderr,
		},
	)
	if exitCode != 0 {
		t.Fatalf("Execute() exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	want := []string{
		"load",
		"prepare DATABASE_URL=configured",
		"start",
		"execute",
		"stop",
	}
	if !equalEvents(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestCommandRejectsAnExecutionWithoutRunner(t *testing.T) {
	executed := false
	adapter, err := migrationsservice.New(migrationsservice.Options[struct{}]{
		Summary: "run database migrations",
		Load: func(context.Context, service.Invocation) (struct{}, error) {
			return struct{}{}, nil
		},
		Prepare: func(
			context.Context,
			service.BuildContext,
			struct{},
		) (migrationsservice.Execution, error) {
			return migrationsservice.Execution{}, nil
		},
		Execute: func(context.Context, *migrations.Runner) error {
			executed = true

			return nil
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var stderr bytes.Buffer
	exitCode := service.Execute(
		context.Background(),
		service.Definition{
			Identity: service.Identity{Name: "postal"},
			Commands: service.Commands{Migrate: adapter.Command()},
		},
		service.Invocation{
			Args: []string{"migrate"}, Stdout: io.Discard, Stderr: &stderr,
		},
	)
	if exitCode == 0 {
		t.Fatal("Execute() exit code = 0, want construction failure")
	}
	if executed {
		t.Fatal("Execute() ran without a migration runner")
	}
}

func TestCommandPreservesPreparationAndExecutionFailures(t *testing.T) {
	prepareErr := errors.New("prepare failed")
	executeErr := errors.New("execute failed")
	tests := []struct {
		name       string
		prepareErr error
		executeErr error
	}{
		{name: "prepare", prepareErr: prepareErr},
		{name: "execute", executeErr: executeErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := migrationsservice.New(migrationsservice.Options[struct{}]{
				Summary: "run database migrations",
				Load: func(context.Context, service.Invocation) (struct{}, error) {
					return struct{}{}, nil
				},
				Prepare: func(
					context.Context,
					service.BuildContext,
					struct{},
				) (migrationsservice.Execution, error) {
					return migrationsservice.Execution{
						Runner: testRunner(t),
					}, test.prepareErr
				},
				Execute: func(context.Context, *migrations.Runner) error {
					return test.executeErr
				},
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			var stderr bytes.Buffer
			exitCode := service.Execute(
				context.Background(),
				service.Definition{
					Identity: service.Identity{Name: "postal"},
					Commands: service.Commands{Migrate: adapter.Command()},
				},
				service.Invocation{
					Args:   []string{"migrate"},
					Stdout: io.Discard, Stderr: &stderr,
				},
			)
			if exitCode == 0 {
				t.Fatalf("Execute() exit code = 0, want %s failure", test.name)
			}
		})
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	validLoad := func(context.Context, service.Invocation) (struct{}, error) {
		return struct{}{}, nil
	}
	validPrepare := func(
		context.Context,
		service.BuildContext,
		struct{},
	) (migrationsservice.Execution, error) {
		return migrationsservice.Execution{}, nil
	}
	validExecute := func(context.Context, *migrations.Runner) error { return nil }
	tests := []migrationsservice.Options[struct{}]{
		{Load: validLoad, Prepare: validPrepare, Execute: validExecute},
		{Summary: "migrate", Prepare: validPrepare, Execute: validExecute},
		{Summary: "migrate", Load: validLoad, Execute: validExecute},
		{Summary: "migrate", Load: validLoad, Prepare: validPrepare},
	}
	for _, options := range tests {
		_, err := migrationsservice.New(options)
		if !errors.Is(err, migrationsservice.ErrInvalidOptions) {
			t.Fatalf("New() error = %v, want ErrInvalidOptions", err)
		}
		var optionsError *migrationsservice.OptionsError
		if !errors.As(err, &optionsError) || optionsError.Error() == "" {
			t.Fatalf("New() error = %v, want OptionsError", err)
		}
	}
}

func TestExecutionErrorUsesSafeDiagnostic(t *testing.T) {
	err := &migrationsservice.ExecutionError{
		Field: "Runner", Reason: "must not be nil",
	}
	if err.Error() !=
		"Runner: must not be nil: invalid migrations service execution" {
		t.Fatalf("ExecutionError.Error() = %q", err.Error())
	}
	if !errors.Is(err, migrationsservice.ErrInvalidExecution) {
		t.Fatal("ExecutionError did not retain its stable classification")
	}
}

type source struct{}

func (source) Load(context.Context) ([]migrations.Migration, error) {
	return nil, nil
}

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
