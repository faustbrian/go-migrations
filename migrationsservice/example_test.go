package migrationsservice_test

import (
	"context"
	"fmt"
	"io"

	"github.com/faustbrian/golib/pkg/migrations"
	"github.com/faustbrian/golib/pkg/migrations/migrationsservice"
	"github.com/faustbrian/golib/pkg/service"
)

func ExampleNew() {
	runner, _ := migrations.NewRunner(source{}, backend{})
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
			return migrationsservice.Execution{Runner: runner}, nil
		},
		Execute: func(context.Context, *migrations.Runner) error {
			return nil
		},
	})
	if err != nil {
		fmt.Println("adapter setup failed")

		return
	}

	exitCode := service.Execute(
		context.Background(),
		service.Definition{
			Identity: service.Identity{Name: "orders"},
			Commands: service.Commands{Migrate: adapter.Command()},
		},
		service.Invocation{
			Args: []string{"migrate"}, Stdout: io.Discard, Stderr: io.Discard,
		},
	)
	fmt.Println(exitCode)
	// Output: 0
}
