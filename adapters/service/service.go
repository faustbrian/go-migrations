// Package migrationsservice adapts an explicit migrations.Runner to the
// standard Service v3 migrate command.
//
// The adapter owns no database or migration semantics. Callers load their
// configuration, construct the runner and resource components, and select the
// runner operation. The adapter fixes only the command name, one-shot role,
// task boundary, and cleanup ordering required by service. Callbacks run under
// service contexts and retain their errors; the adapter does not contain their
// panics or provide a new authorization/redaction boundary. See
// TestCommandPreservesOneShotOrderingAndCallerOwnership in service_test.go.
package migrationsservice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	migrations "github.com/faustbrian/go-migrations/v5"
	service "github.com/faustbrian/go-service/v3"
)

var (
	// ErrInvalidOptions identifies invalid adapter construction.
	ErrInvalidOptions = errors.New("invalid migrations service options")
	// ErrInvalidExecution identifies a prepared migration without a runner.
	ErrInvalidExecution = errors.New("invalid migrations service execution")
)

// Load resolves caller-owned migration configuration before resource construction.
type Load[C any] func(context.Context, service.Invocation) (C, error)

// Prepare constructs only the resources needed by the migrate role.
type Prepare[C any] func(context.Context, service.BuildContext, C) (Execution, error)

// Execute selects one caller-owned migrations.Runner operation.
type Execute func(context.Context, *migrations.Runner) error

// Execution contains the prepared runner and its explicit resource lifecycle.
type Execution struct {
	// Runner is the concrete migration runner used by Execute.
	Runner *migrations.Runner
	// Components acquire migration-only dependencies and release them after the one-shot task finishes.
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

// Adapter retains one immutable migrate command definition.
type Adapter[C any] struct {
	summary string
	load    Load[C]
	prepare Prepare[C]
	execute Execute
}

// New validates and constructs the standard migrate command adapter.
func New[C any](options Options[C]) (*Adapter[C], error) {
	if err := validateOptions(options.Summary, options.Load != nil, options.Prepare != nil, options.Execute != nil); err != nil {
		return nil, err
	}
	return &Adapter[C]{summary: options.Summary, load: options.Load, prepare: options.Prepare, execute: options.Execute}, nil
}

func validateOptions(summary string, load, prepare, execute bool) error {
	if strings.TrimSpace(summary) == "" {
		return &OptionsError{Field: "Summary", Reason: "must not be blank"}
	}
	if !load {
		return &OptionsError{Field: "Load", Reason: "must not be nil"}
	}
	if !prepare {
		return &OptionsError{Field: "Prepare", Reason: "must not be nil"}
	}
	if !execute {
		return &OptionsError{Field: "Execute", Reason: "must not be nil"}
	}
	return nil
}

// Command returns the one-shot migrate registration. Preparation/execution
// errors are retained; diagnostics safety belongs to those caller callbacks.
// Components start before the task and stop under the service lifecycle, while
// the adapter copies the component slice rather than owning the runner/database.
func (adapter *Adapter[C]) Command() service.Command {
	return service.CommandFor(service.CommandSpec[C]{
		Name: "migrate", Summary: adapter.summary, Kind: service.CommandKindOneShot, Load: adapter.load,
		Build: func(ctx context.Context, build service.BuildContext, configuration C) (service.Plan, error) {
			execution, err := adapter.prepare(ctx, build, configuration)
			if err != nil {
				return service.Plan{}, err
			}
			if execution.Runner == nil {
				return service.Plan{}, &ExecutionError{Field: "Runner", Reason: "must not be nil"}
			}
			components := append([]service.Component(nil), execution.Components...)
			return service.Plan{
				Components: components,
				Tasks: []service.Task{{Name: "migrate", Run: func(ctx context.Context) error {
					return adapter.execute(ctx, execution.Runner)
				}}},
			}, nil
		},
	})
}
