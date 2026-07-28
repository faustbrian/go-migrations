// Package migrationsservice adapts an explicit migrations.Runner to the
// standard service migrate command.
//
// The adapter owns no database or migration semantics. Callers load their
// configuration, construct the runner and resource components, and select the
// runner operation. The adapter fixes only the command name, one-shot role,
// task boundary, and cleanup ordering required by service.
package migrationsservice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/faustbrian/golib/pkg/migrations"
	"github.com/faustbrian/golib/pkg/service"
)

var (
	// ErrInvalidOptions identifies invalid adapter construction.
	ErrInvalidOptions = errors.New("invalid migrations service options")
	// ErrInvalidExecution identifies a prepared migration without a runner.
	ErrInvalidExecution = errors.New("invalid migrations service execution")
)

// Load resolves caller-owned migration configuration before resource
// construction.
type Load[C any] func(context.Context, service.Invocation) (C, error)

// Prepare constructs only the resources needed by the migrate role.
type Prepare[C any] func(
	context.Context,
	service.BuildContext,
	C,
) (Execution, error)

// Execute selects one caller-owned migrations.Runner operation.
type Execute func(context.Context, *migrations.Runner) error

// Execution contains the prepared runner and its explicit resource lifecycle.
type Execution struct {
	// Runner is the concrete migration runner used by Execute.
	Runner *migrations.Runner
	// Components acquire migration-only dependencies and release them after the
	// one-shot task finishes.
	Components []service.Component
}

// Options configure the standard migrate command adapter.
type Options[C any] struct {
	// Summary is the one-line migrate command help text.
	Summary string
	// Load resolves typed migration configuration.
	Load Load[C]
	// Prepare constructs the runner and migration-only components.
	Prepare Prepare[C]
	// Execute selects the runner operation without adapter-owned semantics.
	Execute Execute
}

// OptionsError identifies one rejected option.
type OptionsError struct {
	// Field identifies the rejected option.
	Field string
	// Reason describes the safe failure category.
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
	// Field identifies the invalid execution field.
	Field string
	// Reason describes the safe failure category.
	Reason string
}

// Error returns a secret-safe execution diagnostic.
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
	if strings.TrimSpace(options.Summary) == "" {
		return nil, &OptionsError{Field: "Summary", Reason: "must not be blank"}
	}
	if options.Load == nil {
		return nil, &OptionsError{Field: "Load", Reason: "must not be nil"}
	}
	if options.Prepare == nil {
		return nil, &OptionsError{Field: "Prepare", Reason: "must not be nil"}
	}
	if options.Execute == nil {
		return nil, &OptionsError{Field: "Execute", Reason: "must not be nil"}
	}

	return &Adapter[C]{
		summary: options.Summary,
		load:    options.Load,
		prepare: options.Prepare,
		execute: options.Execute,
	}, nil
}

// Command returns the standard one-shot migrate registration.
func (adapter *Adapter[C]) Command() service.Command {
	return service.CommandFor(service.CommandSpec[C]{
		Name:    "migrate",
		Summary: adapter.summary,
		Kind:    service.CommandKindOneShot,
		Load:    adapter.load,
		Build: func(
			ctx context.Context,
			build service.BuildContext,
			configuration C,
		) (service.Plan, error) {
			execution, err := adapter.prepare(ctx, build, configuration)
			if err != nil {
				return service.Plan{}, err
			}
			if execution.Runner == nil {
				return service.Plan{}, &ExecutionError{
					Field: "Runner", Reason: "must not be nil",
				}
			}
			components := append(
				[]service.Component(nil),
				execution.Components...,
			)

			return service.Plan{
				Components: components,
				Tasks: []service.Task{{
					Name: "migrate",
					Run: func(ctx context.Context) error {
						return adapter.execute(ctx, execution.Runner)
					},
				}},
			}, nil
		},
	})
}
