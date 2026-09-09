//lint:file-ignore SA1019 Compatibility coverage requires the deprecated import.

package migrations_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	migrations "github.com/faustbrian/go-migrations"
	canonical "github.com/faustbrian/go-migrations/adapters/service"
	legacy "github.com/faustbrian/go-migrations/migrationsservice" //nolint:staticcheck // Compatibility coverage requires the deprecated path.
	service "github.com/faustbrian/go-service"
)

func TestServiceSuccessorPreservesLegacyNamedIdentitiesAndSentinels(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		got  reflect.Type
		want string
	}{
		{"legacy Load", reflect.TypeOf((legacy.Load[int])(nil)), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"legacy Prepare", reflect.TypeOf((legacy.Prepare[int])(nil)), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"legacy Execute", reflect.TypeOf((legacy.Execute)(nil)), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"legacy Execution", reflect.TypeOf(legacy.Execution{}), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"legacy Options", reflect.TypeOf(legacy.Options[int]{}), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"legacy OptionsError", reflect.TypeOf(legacy.OptionsError{}), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"legacy ExecutionError", reflect.TypeOf(legacy.ExecutionError{}), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"legacy Adapter", reflect.TypeOf(legacy.Adapter[int]{}), "github.com/faustbrian/go-migrations/migrationsservice"},
		{"canonical Load", reflect.TypeOf((canonical.Load[int])(nil)), "github.com/faustbrian/go-migrations/adapters/service"},
		{"canonical Prepare", reflect.TypeOf((canonical.Prepare[int])(nil)), "github.com/faustbrian/go-migrations/adapters/service"},
		{"canonical Execute", reflect.TypeOf((canonical.Execute)(nil)), "github.com/faustbrian/go-migrations/adapters/service"},
		{"canonical Execution", reflect.TypeOf(canonical.Execution{}), "github.com/faustbrian/go-migrations/adapters/service"},
		{"canonical Options", reflect.TypeOf(canonical.Options[int]{}), "github.com/faustbrian/go-migrations/adapters/service"},
		{"canonical OptionsError", reflect.TypeOf(canonical.OptionsError{}), "github.com/faustbrian/go-migrations/adapters/service"},
		{"canonical ExecutionError", reflect.TypeOf(canonical.ExecutionError{}), "github.com/faustbrian/go-migrations/adapters/service"},
		{"canonical Adapter", reflect.TypeOf(canonical.Adapter[int]{}), "github.com/faustbrian/go-migrations/adapters/service"},
	} {
		if got := test.got.PkgPath(); got != test.want {
			t.Errorf("%s package = %q, want %q", test.name, got, test.want)
		}
	}
	if legacy.ErrInvalidOptions != canonical.ErrInvalidOptions ||
		legacy.ErrInvalidExecution != canonical.ErrInvalidExecution {
		t.Fatal("legacy and canonical sentinels differ")
	}
}

func TestLegacyServiceFacadePreservesErrorsAndCommandBehavior(t *testing.T) {
	t.Parallel()
	_, err := legacy.New(legacy.Options[int]{})
	var optionsError *legacy.OptionsError
	if !errors.Is(err, legacy.ErrInvalidOptions) || !errors.As(err, &optionsError) {
		t.Fatalf("legacy New() error = %v", err)
	}

	runner, err := migrations.NewRunner(successorSource{}, successorBackend{})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := legacy.New(legacy.Options[int]{
		Summary: "migrate",
		Load:    func(context.Context, service.Invocation) (int, error) { return 1, nil },
		Prepare: func(context.Context, service.BuildContext, int) (legacy.Execution, error) {
			return legacy.Execution{Runner: runner}, nil
		},
		Execute: func(_ context.Context, actual *migrations.Runner) error {
			if actual != runner {
				t.Fatal("legacy facade replaced the caller-owned runner")
			}
			return nil
		},
	})
	if err != nil || reflect.ValueOf(adapter.Command()).IsZero() {
		t.Fatalf("legacy Command() = %#v, %v", adapter, err)
	}
}

type successorSource struct{}

func (successorSource) Load(context.Context) ([]migrations.Migration, error) { return nil, nil }

type successorBackend struct{}

func (successorBackend) Acquire(context.Context) (migrations.Session, error) {
	return nil, errors.New("not used")
}
