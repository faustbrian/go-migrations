//lint:file-ignore SA1019 Compatibility coverage requires the deprecated import.

package migrations_test

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	correlation "github.com/faustbrian/go-correlation"
	migrations "github.com/faustbrian/go-migrations/v3"
	canonical "github.com/faustbrian/go-migrations/v3/adapters/service"
	legacy "github.com/faustbrian/go-migrations/v3/migrationsservice" //nolint:staticcheck // Compatibility coverage requires the deprecated path.
	service "github.com/faustbrian/go-service"
)

const entropyChildEnvironment = "GOLIB_MIGRATIONS_ENTROPY_CHILD"
const entropyDiagnostic = "reader-specific diagnostic"

// Entropy replacement stays in a child so other tests never observe global state.
func TestServiceDependencyEntropyFailure(t *testing.T) {
	if os.Getenv(entropyChildEnvironment) == "1" {
		testDefaultFactoryEntropyFailure(t)
		testMigrateEntropyFailure(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceDependencyEntropyFailure$")
	command.Env = append(os.Environ(), entropyChildEnvironment+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated entropy contract failed: %v\n%s", err, output)
	}
}

func testDefaultFactoryEntropyFailure(t *testing.T) {
	t.Helper()
	reader := &failingEntropyReader{cause: errors.New(entropyDiagnostic)}
	cryptorand.Reader = reader
	factory, err := correlation.NewFactory(correlation.FactoryOptions{})
	if err != nil {
		t.Fatal("default factory construction failed")
	}
	values, err := factory.Create()
	if reader.calls == 0 || values != (correlation.Values{}) || !errors.Is(err, correlation.ErrGeneration) {
		t.Fatal("entropy failure must exercise reader, return zero values and preserve ErrGeneration")
	}
	if errors.Is(err, reader.cause) || errorContainsEntropyDiagnostic(err) {
		t.Error("default UUID generation exposes the entropy reader diagnostic or cause")
	}
}

func testMigrateEntropyFailure(t *testing.T) {
	t.Helper()
	for _, variant := range []string{"canonical", "legacy"} {
		t.Run(variant, func(t *testing.T) {
			reader := &failingEntropyReader{cause: errors.New(entropyDiagnostic)}
			cryptorand.Reader = reader
			load := func(context.Context, service.Invocation) (struct{}, error) {
				t.Error("Load ran after entropy failure")
				return struct{}{}, nil
			}
			execute := func(context.Context, *migrations.Runner) error {
				t.Error("Execute ran after entropy failure")
				return nil
			}
			var command service.Command
			if variant == "canonical" {
				adapter, err := canonical.New(canonical.Options[struct{}]{
					Summary: "migrate", Load: load, Execute: execute,
					Prepare: func(context.Context, service.BuildContext, struct{}) (canonical.Execution, error) {
						t.Error("Prepare ran after entropy failure")
						return canonical.Execution{}, nil
					},
				})
				if err != nil {
					t.Fatal("canonical adapter construction failed")
				}
				command = adapter.Command()
			} else {
				adapter, err := legacy.New(legacy.Options[struct{}]{
					Summary: "migrate", Load: load, Execute: execute,
					Prepare: func(context.Context, service.BuildContext, struct{}) (legacy.Execution, error) {
						t.Error("Prepare ran after entropy failure")
						return legacy.Execution{}, nil
					},
				})
				if err != nil {
					t.Fatal("legacy adapter construction failed")
				}
				command = adapter.Command()
			}
			var stdout, stderr bytes.Buffer
			code := service.Execute(context.Background(), service.Definition{
				Identity: service.Identity{Name: "postal"}, Commands: service.Commands{Migrate: command},
			}, service.Invocation{Args: []string{"migrate"}, Stdout: &stdout, Stderr: &stderr})
			if reader.calls == 0 || code != 70 || stdout.Len() != 0 ||
				!strings.Contains(stderr.String(), "construct migrate failed") || strings.Contains(stderr.String(), entropyDiagnostic) {
				t.Fatal("migrate must fail safely before callbacks when default identity generation fails")
			}
		})
	}
}

func TestServiceDependencyDefaultUUIDs(t *testing.T) {
	factory, err := correlation.NewFactory(correlation.FactoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	values, err := factory.Create()
	if err != nil {
		t.Fatal("healthy default generation failed")
	}
	canonicalUUID := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !canonicalUUID.MatchString(values.CorrelationID.String()) || !canonicalUUID.MatchString(values.RequestID.String()) ||
		values.CorrelationID.String() == values.RequestID.String() {
		t.Fatal("default generation must produce distinct canonical lowercase UUIDv4 IDs")
	}
}

type failingEntropyReader struct {
	calls int
	cause error
}

func (reader *failingEntropyReader) Read([]byte) (int, error) {
	reader.calls++
	return 0, reader.cause
}

func errorContainsEntropyDiagnostic(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), entropyDiagnostic) {
		return true
	}
	if multiple, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range multiple.Unwrap() {
			if errorContainsEntropyDiagnostic(cause) {
				return true
			}
		}
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return errorContainsEntropyDiagnostic(single.Unwrap())
	}
	return false
}
