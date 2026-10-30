package migrationsservice_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	migrations "github.com/faustbrian/go-migrations/v5"
	migrationsservice "github.com/faustbrian/go-migrations/v5/adapters/service"
	service "github.com/faustbrian/go-service/v3"
)

func TestMigrationPreservesSelectedOperationAndCleanup(t *testing.T) {
	for _, failure := range []string{"none", "load", "prepare", "runner", "start", "execute", "stop", "cancel", "cancel+stop"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := testRunner(t)
			events := []string{}
			fail := errors.New("selected failure")
			adapter, err := migrationsservice.New(migrationsservice.Options[string]{
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
				Prepare: func(ctx context.Context, build service.BuildContext, configuration string) (migrationsservice.Execution, error) {
					events = append(events, "prepare")
					if ctx.Err() != nil || build.Identity.Name != "postal" || build.Identity.Role != "migrate" || configuration != "DATABASE_URL="+environmentMarker {
						t.Fatalf("prepare inputs: %v, %#v, %q", ctx.Err(), build.Identity, configuration)
					}
					if failure == "prepare" {
						return migrationsservice.Execution{}, fail
					}
					execution := migrationsservice.Execution{Runner: runner, Components: []service.Component{{
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
							if failure == "stop" || failure == "cancel+stop" {
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
					if failure == "cancel" || failure == "cancel+stop" {
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
			var output bytes.Buffer
			code := service.Execute(ctx, service.Definition{
				Identity: service.Identity{Name: "postal"}, Commands: service.Commands{Migrate: adapter.Command()},
			}, service.Invocation{Args: []string{"migrate"}, Environment: []string{"DATABASE_URL=" + environmentMarker}, Stdout: &output, Stderr: &output})
			if strings.Contains(output.String(), environmentMarker) {
				t.Fatalf("%s diagnostics disclosed an environment value", failure)
			}
			// Service classifies cooperative parent cancellation as a clean exit
			// only when cleanup succeeds. The migration adapter must retain that
			// classification and still join the task before cleanup with an
			// independent, bounded context. The other codes characterize Service v3
			// classification for a migrate-only definition, which the compatibility
			// guide promises.
			wantCodes := map[string]int{
				"none": 0, "cancel": 0, "cancel+stop": 70, "load": 78, "prepare": 70,
				"runner": 70, "start": 75, "execute": 1, "stop": 70,
			}
			if code != wantCodes[failure] {
				t.Fatalf("%s exit = %d, want %d", failure, code, wantCodes[failure])
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

func TestCommandStartsComponentsInOrderAndStopsInReverse(t *testing.T) {
	events := []string{}
	component := func(name string) service.Component {
		return service.Component{
			Name:  name,
			Start: func(context.Context) error { events = append(events, "start "+name); return nil },
			Stop:  func(context.Context) error { events = append(events, "stop "+name); return nil },
		}
	}
	runner := testRunner(t)
	adapter, err := migrationsservice.New(migrationsservice.Options[struct{}]{
		Summary: "migrate",
		Load:    func(context.Context, service.Invocation) (struct{}, error) { return struct{}{}, nil },
		Prepare: func(context.Context, service.BuildContext, struct{}) (migrationsservice.Execution, error) {
			return migrationsservice.Execution{Runner: runner, Components: []service.Component{component("a"), component("b")}}, nil
		},
		Execute: func(context.Context, *migrations.Runner) error { events = append(events, "execute"); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := service.Execute(context.Background(), service.Definition{
		Identity: service.Identity{Name: "postal"}, Commands: service.Commands{Migrate: adapter.Command()},
	}, service.Invocation{Args: []string{"migrate"}, Stdout: &output, Stderr: &output}); code != 0 {
		t.Fatalf("exit = %d: %s", code, output.String())
	}
	want := []string{"start a", "start b", "execute", "stop b", "stop a"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

const environmentMarker = "migration-environment-marker"

func TestExecutionErrorUsesSafeDiagnostic(t *testing.T) {
	err := &migrationsservice.ExecutionError{Field: "Runner", Reason: "must not be nil"}
	if err.Error() != "Runner: must not be nil: invalid migrations service execution" {
		t.Fatalf("ExecutionError.Error() = %q", err.Error())
	}
	if !errors.Is(err, migrationsservice.ErrInvalidExecution) {
		t.Fatal("ExecutionError did not retain its stable classification")
	}
}
