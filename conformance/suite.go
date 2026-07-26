// Package conformance provides the reusable behavioral test suite every
// migration backend must pass without depending on its execution engine.
package conformance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

// Harness supplies engine-specific SQL and isolated runner construction.
// NewRunner must return runners sharing one fresh database when called more
// than once with the same *testing.T and isolated databases for different
// subtests. Exec is used only to remove reviewed partial effects before an
// explicit rolled-back recovery decision.
type Harness struct {
	NewRunner            func(*testing.T, fstest.MapFS) *migrations.Runner
	Exec                 func(*testing.T, string)
	TransactionalUp      string
	TransactionalDown    string
	NoTransactionUp      string
	NoTransactionDown    string
	TransactionalFailure string
	NoTransactionFailure string
	RemovePartialEffects string
}

// Run executes the engine-neutral migration behavior contract.
func Run(t *testing.T, harness Harness) {
	t.Helper()
	validateHarness(t, harness)

	t.Run("apply status idempotency and rollback", func(t *testing.T) {
		files := migrationFiles(harness)
		runner := harness.NewRunner(t, files)
		plan, err := runner.Plan(context.Background())
		if err != nil {
			t.Fatalf("Plan() error = %v", err)
		}
		if steps := plan.Steps(); len(steps) != 2 ||
			steps[0].Action() != migrations.ActionApply ||
			steps[0].Migration().Version() != 1 ||
			steps[1].Action() != migrations.ActionApply ||
			steps[1].Migration().Version() != 2 {
			t.Fatalf("initial Plan() = %#v", steps)
		}
		status, err := runner.Status(context.Background())
		if err != nil {
			t.Fatalf("initial Status() error = %v", err)
		}
		for _, entry := range status.Entries() {
			if entry.State() != migrations.StatePending {
				t.Fatalf("initial status entry = %#v, want pending", entry)
			}
		}
		result, err := runner.Up(context.Background())
		if err != nil {
			t.Fatalf("Up() error = %v", err)
		}
		if len(result.Records()) != 2 {
			t.Fatalf("Up() records = %d, want 2", len(result.Records()))
		}
		second, err := runner.Up(context.Background())
		if err != nil {
			t.Fatalf("second Up() error = %v", err)
		}
		if len(second.Records()) != 0 {
			t.Fatalf("second Up() records = %d, want 0", len(second.Records()))
		}
		plan, err = runner.Plan(context.Background())
		if err != nil {
			t.Fatalf("applied Plan() error = %v", err)
		}
		if steps := plan.Steps(); len(steps) != 0 {
			t.Fatalf("applied Plan() = %#v, want empty", steps)
		}
		status, err = runner.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		for _, entry := range status.Entries() {
			if entry.State() != migrations.StateApplied {
				t.Fatalf("status entry = %#v, want applied", entry)
			}
		}
		if _, err := runner.Down(context.Background(), 2); err != nil {
			t.Fatalf("Down() error = %v", err)
		}
	})

	t.Run("source and ledger divergence fails every read and execution path", func(t *testing.T) {
		original := fstest.MapFS{
			"migrations/000001_conformance_history.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\n" + harness.TransactionalUp + "\n",
			)},
		}
		cases := []struct {
			name   string
			files  fstest.MapFS
			target error
		}{
			{
				name: "checksum mutation",
				files: fstest.MapFS{
					"migrations/000001_conformance_history.sql": &fstest.MapFile{Data: []byte(
						"-- +migrations Up\n" + harness.TransactionalUp + "\n-- changed after apply\n",
					)},
				},
				target: migrations.ErrChecksumMismatch,
			},
			{
				name: "rename",
				files: fstest.MapFS{
					"migrations/000001_renamed_history.sql": &fstest.MapFile{Data: []byte(
						"-- +migrations Up\n" + harness.TransactionalUp + "\n",
					)},
				},
				target: migrations.ErrRenamedMigration,
			},
			{
				name: "deletion",
				files: fstest.MapFS{
					"migrations/000002_pending.sql": &fstest.MapFile{Data: []byte(
						"-- +migrations Up\nSELECT 1;\n",
					)},
				},
				target: migrations.ErrDeletedMigration,
			},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				if _, err := harness.NewRunner(t, original).Up(context.Background()); err != nil {
					t.Fatalf("seed historical migration: %v", err)
				}
				runner := harness.NewRunner(t, test.files)
				assertHistoryError(t, test.target, func() error {
					_, err := runner.Plan(context.Background())

					return err
				})
				assertHistoryError(t, test.target, func() error {
					_, err := runner.Status(context.Background())

					return err
				})
				assertHistoryError(t, test.target, func() error {
					_, err := runner.Up(context.Background())

					return err
				})
			})
		}
	})

	t.Run("concurrent runners serialize", func(t *testing.T) {
		files := fstest.MapFS{
			"migrations/000001_concurrent.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\n" + harness.TransactionalUp + "\n",
			)},
		}
		first := harness.NewRunner(t, files)
		second := harness.NewRunner(t, files)
		results := make(chan migrations.Result, 2)
		errorsFound := make(chan error, 2)
		var wait sync.WaitGroup
		for _, runner := range []*migrations.Runner{first, second} {
			wait.Add(1)
			go func(runner *migrations.Runner) {
				defer wait.Done()
				result, err := runner.Up(context.Background())
				results <- result
				errorsFound <- err
			}(runner)
		}
		wait.Wait()
		close(results)
		close(errorsFound)
		for err := range errorsFound {
			if err != nil {
				t.Fatalf("concurrent Up() error = %v", err)
			}
		}
		completed := 0
		for result := range results {
			completed += len(result.Records())
		}
		if completed != 1 {
			t.Fatalf("completed migrations = %d, want 1", completed)
		}
	})

	t.Run("transaction failure remains pending", func(t *testing.T) {
		runner := harness.NewRunner(t, fstest.MapFS{
			"migrations/000001_fail.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations Up\n" + harness.TransactionalFailure + "\n",
			)},
		})
		if _, err := runner.Up(context.Background()); err == nil {
			t.Fatal("Up() error = nil, want transactional failure")
		}
		status, err := runner.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if len(status.Entries()) != 1 || status.Entries()[0].State() != migrations.StatePending {
			t.Fatalf("Status() = %#v, want pending", status.Entries())
		}
	})

	t.Run("no transaction failure is dirty and recoverable", func(t *testing.T) {
		files := fstest.MapFS{
			"migrations/000001_partial.sql": &fstest.MapFile{Data: []byte(
				"-- +migrations NoTransaction\n-- +migrations Up\n" +
					harness.NoTransactionFailure + "\n",
			)},
		}
		runner := harness.NewRunner(t, files)
		if _, err := runner.Up(context.Background()); err == nil {
			t.Fatal("Up() error = nil, want partial failure")
		}
		status, err := runner.Status(context.Background())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if len(status.Entries()) != 1 || status.Entries()[0].State() != migrations.StateDirty {
			t.Fatalf("Status() = %#v, want dirty", status.Entries())
		}
		harness.Exec(t, harness.RemovePartialEffects)
		source, err := migrations.NewFSSource(files, "migrations")
		if err != nil {
			t.Fatalf("NewFSSource() error = %v", err)
		}
		loaded, err := source.Load(context.Background())
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		recovery, err := migrations.NewRecovery(
			loaded[0].Version(),
			loaded[0].Checksum(),
			migrations.RecoveryMarkRolledBack,
		)
		if err != nil {
			t.Fatalf("NewRecovery() error = %v", err)
		}
		if _, err := runner.Recover(context.Background(), recovery); err != nil {
			t.Fatalf("Recover() error = %v", err)
		}
	})
}

func assertHistoryError(t *testing.T, target error, operation func() error) {
	t.Helper()
	if err := operation(); !errors.Is(err, target) {
		t.Fatalf("operation error = %v, want %v", err, target)
	}
}

func validateHarness(t *testing.T, harness Harness) {
	t.Helper()
	if harness.NewRunner == nil || harness.Exec == nil ||
		harness.TransactionalUp == "" || harness.TransactionalDown == "" ||
		harness.NoTransactionUp == "" || harness.NoTransactionDown == "" ||
		harness.TransactionalFailure == "" || harness.NoTransactionFailure == "" ||
		harness.RemovePartialEffects == "" {
		t.Fatal(errors.New("incomplete migration engine conformance harness"))
	}
}

func migrationFiles(harness Harness) fstest.MapFS {
	return fstest.MapFS{
		"migrations/000001_transactional.sql": &fstest.MapFile{Data: []byte(
			"-- +migrations Up\n" + harness.TransactionalUp + "\n" +
				"-- +migrations Down\n" + harness.TransactionalDown + "\n",
		)},
		"migrations/000002_no_transaction.sql": &fstest.MapFile{Data: []byte(
			"-- +migrations NoTransaction\n-- +migrations Up\n" +
				harness.NoTransactionUp + "\n-- +migrations Down\n" +
				harness.NoTransactionDown + "\n",
		)},
	}
}
