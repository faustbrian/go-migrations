package migrationsservice_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	migrations "github.com/faustbrian/go-migrations/v3"
	migrationsservice "github.com/faustbrian/go-migrations/v3/adapters/service"
	service "github.com/faustbrian/go-service/v2"
)

func TestServiceV2MigrationPreservesSelectedOperationAndCleanup(t *testing.T) {
	for _, failure := range []string{"none", "load", "prepare", "runner", "start", "execute", "stop", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := testRunner(t)
			events := []string{}
			fail := errors.New("selected failure")
			adapter, err := migrationsservice.NewV2(migrationsservice.OptionsV2[string]{
				Summary: "run database migrations",
				Load: func(ctx context.Context, invocation service.Invocation) (string, error) {
					events = append(events, "load")
					if ctx.Err() != nil {
						t.Fatal("load context canceled")
					}
					if failure == "load" {
						return "", fail
					}
					return invocation.Environment[0], nil
				},
				Prepare: func(ctx context.Context, build service.BuildContext, configuration string) (migrationsservice.ExecutionV2, error) {
					events = append(events, "prepare")
					if ctx.Err() != nil || build.Identity.Name != "postal" || build.Identity.Role != "migrate" || configuration != "DATABASE_URL=configured" {
						t.Fatalf("prepare inputs: %v, %#v, %q", ctx.Err(), build.Identity, configuration)
					}
					if failure == "prepare" {
						return migrationsservice.ExecutionV2{}, fail
					}
					execution := migrationsservice.ExecutionV2{Runner: runner, Components: []service.Component{{
						Name: "database",
						Start: func(context.Context) error {
							events = append(events, "start")
							if failure == "start" {
								return fail
							}
							return nil
						},
						Stop: func(ctx context.Context) error {
							events = append(events, "stop")
							if _, bounded := ctx.Deadline(); ctx.Err() != nil || !bounded {
								t.Errorf("cleanup context is not live and bounded: %v", ctx.Err())
							}
							if failure == "stop" {
								return fail
							}
							return nil
						},
					}}}
					if failure == "runner" {
						execution.Runner = nil
					}
					return execution, nil
				},
				Execute: func(ctx context.Context, actual *migrations.Runner) error {
					events = append(events, "execute")
					if actual != runner {
						t.Error("selected runner changed")
						return errors.New("unexpected runner")
					}
					if failure == "cancel" {
						cancel()
						return ctx.Err()
					}
					if failure == "execute" {
						return fail
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			code := service.Execute(ctx, service.Definition{
				Identity: service.Identity{Name: "postal"}, Commands: service.Commands{Migrate: adapter.Command()},
			}, service.Invocation{Args: []string{"migrate"}, Environment: []string{"DATABASE_URL=configured"}, Stdout: io.Discard, Stderr: io.Discard})
			// Service classifies cooperative parent cancellation as a clean exit.
			// The migration adapter must retain that classification and still join
			// the task before cleanup with an independent, bounded context.
			if (failure == "none" || failure == "cancel") && code != 0 {
				t.Fatalf("successful migration exit = %d", code)
			}
			if failure != "none" && failure != "cancel" && code == 0 {
				t.Fatalf("%s failure reported success", failure)
			}
			want := []string{"load", "prepare", "start", "execute", "stop"}
			switch failure {
			case "load":
				want = want[:1]
			case "prepare", "runner":
				want = want[:2]
			case "start":
				want = want[:3]
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events = %v, want %v", events, want)
			}
		})
	}
}

func TestServiceV2RejectsInvalidCommandOptions(t *testing.T) {
	load := func(context.Context, service.Invocation) (struct{}, error) { return struct{}{}, nil }
	prepare := func(context.Context, service.BuildContext, struct{}) (migrationsservice.ExecutionV2, error) {
		return migrationsservice.ExecutionV2{}, nil
	}
	execute := func(context.Context, *migrations.Runner) error { return nil }
	for _, options := range []migrationsservice.OptionsV2[struct{}]{
		{Load: load, Prepare: prepare, Execute: execute},
		{Summary: "migrate", Prepare: prepare, Execute: execute},
		{Summary: "migrate", Load: load, Execute: execute},
		{Summary: "migrate", Load: load, Prepare: prepare},
	} {
		_, err := migrationsservice.NewV2(options)
		var invalid *migrationsservice.OptionsError
		if !errors.Is(err, migrationsservice.ErrInvalidOptions) || !errors.As(err, &invalid) {
			t.Fatalf("invalid options error = %v", err)
		}
	}
}
