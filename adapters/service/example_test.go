package migrationsservice_test

import (
	"context"
	"fmt"
	"io"

	migrations "github.com/faustbrian/go-migrations/v4"
	migrationsservice "github.com/faustbrian/go-migrations/v4/adapters/service"
	service "github.com/faustbrian/go-service/v2"
)

// ExampleNew registers a one-shot migrate command whose caller prepares the
// runner and selects the operation. The in-memory runner stands in for a
// PostgreSQL backend; a real job would call runner.Up or another operation.
func ExampleNew() {
	adapter, err := migrationsservice.New(migrationsservice.Options[string]{
		Summary: "apply database migrations",
		Load: func(_ context.Context, invocation service.Invocation) (string, error) {
			return "configured", nil
		},
		Prepare: func(context.Context, service.BuildContext, string) (migrationsservice.Execution, error) {
			runner, err := migrations.NewRunner(source{}, backend{})
			return migrationsservice.Execution{Runner: runner}, err
		},
		Execute: func(context.Context, *migrations.Runner) error {
			fmt.Println("migrate task ran")
			return nil
		},
	})
	if err != nil {
		fmt.Println("adapter setup failed")
		return
	}
	code := service.Execute(context.Background(), service.Definition{
		Identity: service.Identity{Name: "example"},
		Commands: service.Commands{Migrate: adapter.Command()},
	}, service.Invocation{Args: []string{"migrate"}, Stdout: io.Discard, Stderr: io.Discard})
	fmt.Println("exit", code)
	// Output:
	// migrate task ran
	// exit 0
}
