// Package migrationsservice preserves the released service-adapter path and
// named type identities by delegating to adapters/service. It shares sentinels
// and one-shot lifecycle behavior, translating option errors to legacy types.
// It adds no database ownership, execution policy or retries.
//
// Deprecated: use github.com/faustbrian/go-migrations/v3/adapters/service.
package migrationsservice

import (
	"context"
	"errors"
	"fmt"

	migrations "github.com/faustbrian/go-migrations/v3"
	canonical "github.com/faustbrian/go-migrations/v3/adapters/service"
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
	// Runner is required and remains caller-owned; Execute chooses its use.
	Runner *migrations.Runner
	// Components supply the migration-only service lifecycle. Their slice is
	// copied for the plan; resource lifetime follows service start/stop semantics.
	Components []service.Component
}

// Options configure the standard migrate command adapter.
type Options[C any] struct {
	// Summary is nonblank command help text, preserved as supplied.
	Summary string
	// Load resolves caller configuration before resource construction.
	Load Load[C]
	// Prepare supplies a runner and migration-only lifecycle components.
	Prepare Prepare[C]
	// Execute chooses the operation; the adapter does not retry or select it.
	Execute Execute
}

// OptionsError identifies one rejected option.
type OptionsError struct {
	// Field identifies the rejected option or prepared value.
	Field string
	// Reason explains rejection. Both fields appear verbatim in Error text;
	// callers constructing errors themselves must exclude sensitive content.
	Reason string
}

// Error formats Field, Reason and ErrInvalidOptions without redacting them.
func (err *OptionsError) Error() string {
	return fmt.Sprintf("%s: %s: %v", err.Field, err.Reason, ErrInvalidOptions)
}

// Unwrap exposes the stable option classification.
func (err *OptionsError) Unwrap() error { return ErrInvalidOptions }

// ExecutionError identifies an invalid prepared execution.
type ExecutionError struct {
	// Field identifies the rejected option or prepared value.
	Field string
	// Reason explains rejection. Both fields appear verbatim in Error text;
	// callers constructing errors themselves must exclude sensitive content.
	Reason string
}

// Error formats Field, Reason and ErrInvalidExecution without redacting them.
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

// Command returns the one-shot migrate registration. Preparation/execution
// errors are retained; diagnostics safety belongs to those caller callbacks.
// Components start before the task and stop under the service lifecycle, while
// the adapter copies the component slice rather than owning the runner/database.
func (adapter *Adapter[C]) Command() service.Command { return adapter.inner.Command() }

func translateOptionsError(err error) error {
	var canonicalError *canonical.OptionsError
	if !errors.As(err, &canonicalError) {
		return err
	}
	return &OptionsError{Field: canonicalError.Field, Reason: canonicalError.Reason}
}
