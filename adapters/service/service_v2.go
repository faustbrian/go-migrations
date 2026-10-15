package migrationsservice

import (
	"context"

	migrations "github.com/faustbrian/go-migrations/v3"
	service "github.com/faustbrian/go-service/v2"
)

// ExecutionV2 contains a caller-owned runner and its migration-only Service v2
// components. Components start before the selected operation and stop in reverse
// order afterward. The adapter copies the slice, not the resources or callbacks.
type ExecutionV2 struct {
	// Runner is the concrete migration runner supplied to Execute.
	Runner *migrations.Runner
	// Components acquire and release the dependencies required by this command.
	Components []service.Component
}

// OptionsV2 configures the Service v2 migrate command. Callbacks retain their
// resource, context, error and diagnostic-safety contracts; the adapter adds no
// migration policy, retries or authorization boundary.
type OptionsV2[C any] struct {
	// Summary is nonblank command help text, preserved as supplied.
	Summary string
	// Load resolves caller configuration before resource construction.
	Load func(context.Context, service.Invocation) (C, error)
	// Prepare constructs a runner and the migration-only dependency components.
	Prepare func(context.Context, service.BuildContext, C) (ExecutionV2, error)
	// Execute selects one caller-owned Runner operation without retrying it.
	Execute Execute
}

// AdapterV2 retains an immutable Service v2 migrate command definition. Each
// execution calls the supplied callbacks; they must acquire fresh resources or
// safely share caller-owned resources across executions.
type AdapterV2[C any] struct {
	options OptionsV2[C]
}

// NewV2 validates and constructs a migrate adapter using native Service v2
// invocation, construction and component types. Existing New and Options retain
// their Service v1 API and share the same option/error classification.
func NewV2[C any](options OptionsV2[C]) (*AdapterV2[C], error) {
	if err := validateOptions(options.Summary, options.Load != nil, options.Prepare != nil, options.Execute != nil); err != nil {
		return nil, err
	}
	return &AdapterV2[C]{options: options}, nil
}

// Command returns the Service v2 one-shot migrate registration. Preparation must
// provide a runner; acquisition, task cancellation, joining and reverse cleanup
// belong to Service v2. This binding adds no listener, readiness check or retry.
func (adapter *AdapterV2[C]) Command() service.Command {
	return service.CommandFor(service.CommandSpec[C]{
		Name: "migrate", Summary: adapter.options.Summary, Kind: service.CommandKindOneShot,
		Load: adapter.options.Load,
		Build: func(ctx context.Context, build service.BuildContext, configuration C) (service.Plan, error) {
			execution, err := adapter.options.Prepare(ctx, build, configuration)
			if err != nil {
				return service.Plan{}, err
			}
			if execution.Runner == nil {
				return service.Plan{}, &ExecutionError{Field: "Runner", Reason: "must not be nil"}
			}
			return service.Plan{
				Components: append([]service.Component(nil), execution.Components...),
				Tasks: []service.Task{{Name: "migrate", Run: func(ctx context.Context) error {
					return adapter.options.Execute(ctx, execution.Runner)
				}}},
			}, nil
		},
	})
}
