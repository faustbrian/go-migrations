// Package migrationsservice preserves the released service-adapter path.
//
// Deprecated: use github.com/faustbrian/go-migrations/adapters/service.
package migrationsservice

import (
	"context"
	"errors"
	"fmt"

	migrations "github.com/faustbrian/go-migrations"
	canonical "github.com/faustbrian/go-migrations/adapters/service"
	service "github.com/faustbrian/go-service"
)

var (
	// ErrInvalidOptions identifies invalid adapter construction.
	ErrInvalidOptions = canonical.ErrInvalidOptions
	// ErrInvalidExecution identifies a prepared migration without a runner.
	ErrInvalidExecution = canonical.ErrInvalidExecution
)

// Load resolves caller-owned migration configuration before resource construction.
type Load[C any] func(context.Context, service.Invocation) (C, error)

// Prepare constructs only the resources needed by the migrate role.
type Prepare[C any] func(context.Context, service.BuildContext, C) (Execution, error)

// Execute selects one caller-owned migrations.Runner operation.
type Execute func(context.Context, *migrations.Runner) error

// Execution contains the prepared runner and its explicit resource lifecycle.
type Execution struct {
	Runner     *migrations.Runner
	Components []service.Component
}

// Options configure the standard migrate command adapter.
type Options[C any] struct {
	Summary string
	Load    Load[C]
	Prepare Prepare[C]
	Execute Execute
}

// OptionsError identifies one rejected option.
type OptionsError struct {
	Field  string
	Reason string
}

// Error returns a secret-safe construction diagnostic.
func (err *OptionsError) Error() string {
	return fmt.Sprintf("%s: %s: %v", err.Field, err.Reason, ErrInvalidOptions)
}

// Unwrap exposes the stable option classification.
func (err *OptionsError) Unwrap() error { return ErrInvalidOptions }

// ExecutionError identifies an invalid prepared execution.
type ExecutionError struct {
	Field  string
	Reason string
}

// Error returns a secret-safe execution diagnostic.
func (err *ExecutionError) Error() string {
	return fmt.Sprintf("%s: %s: %v", err.Field, err.Reason, ErrInvalidExecution)
}

// Unwrap exposes the stable execution classification.
func (err *ExecutionError) Unwrap() error { return ErrInvalidExecution }

// Adapter preserves the legacy adapter type identity.
type Adapter[C any] struct{ inner *canonical.Adapter[C] }

// New delegates to the canonical service adapter.
func New[C any](options Options[C]) (*Adapter[C], error) {
	var prepare canonical.Prepare[C]
	if options.Prepare != nil {
		prepare = func(ctx context.Context, build service.BuildContext, configuration C) (canonical.Execution, error) {
			execution, prepareErr := options.Prepare(ctx, build, configuration)
			if prepareErr != nil {
				return canonical.Execution{}, prepareErr
			}
			if execution.Runner == nil {
				return canonical.Execution{}, &ExecutionError{Field: "Runner", Reason: "must not be nil"}
			}
			return canonical.Execution{Runner: execution.Runner, Components: execution.Components}, nil
		}
	}
	inner, err := canonical.New(canonical.Options[C]{
		Summary: options.Summary,
		Load:    canonical.Load[C](options.Load),
		Prepare: prepare,
		Execute: canonical.Execute(options.Execute),
	})
	if err != nil {
		return nil, translateOptionsError(err)
	}
	return &Adapter[C]{inner: inner}, nil
}

// Command returns the standard one-shot migrate registration.
func (adapter *Adapter[C]) Command() service.Command { return adapter.inner.Command() }

func translateOptionsError(err error) error {
	var canonicalError *canonical.OptionsError
	if !errors.As(err, &canonicalError) {
		return err
	}
	return &OptionsError{Field: canonicalError.Field, Reason: canonicalError.Reason}
}
