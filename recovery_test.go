package migrations_test

import (
	"context"
	"errors"
	"testing"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func TestRunnerRecoverMarksVerifiedDirtyMigrationApplied(t *testing.T) {
	t.Parallel()

	migration := mustMigration(t, 1, "create_index", "CREATE INDEX CONCURRENTLY users_email_idx ON users (email);\n")
	dirty := mustRecord(t, migrations.RecordKindMigration, migration, true)
	backend := &recordingBackend{records: []migrations.Record{dirty}}
	runner, err := migrations.NewRunner(
		staticSource{migrations: []migrations.Migration{migration}},
		backend,
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	request, err := migrations.NewRecovery(
		migration.Version(),
		migration.Checksum(),
		migrations.RecoveryMarkApplied,
	)
	if err != nil {
		t.Fatalf("NewRecovery() error = %v", err)
	}

	result, err := runner.Recover(context.Background(), request)
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if result.Action() != migrations.RecoveryMarkApplied || result.Record().Dirty() {
		t.Fatalf("Recover() result = %#v", result)
	}
	if got := backend.calls(); !equalStrings(got, []string{
		"acquire",
		"prepare",
		"records",
		"recover:1:applied",
		"release",
	}) {
		t.Fatalf("backend calls = %v", got)
	}
}

func TestRunnerRecoverRejectsUnmatchedOrCleanHistory(t *testing.T) {
	t.Parallel()

	migration := mustMigration(t, 1, "create_index", "SELECT 1;\n")
	clean := mustRecord(t, migrations.RecordKindMigration, migration, false)
	backend := &recordingBackend{records: []migrations.Record{clean}}
	runner, err := migrations.NewRunner(
		staticSource{migrations: []migrations.Migration{migration}},
		backend,
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	request, err := migrations.NewRecovery(
		migration.Version(),
		migration.Checksum(),
		migrations.RecoveryMarkRolledBack,
	)
	if err != nil {
		t.Fatalf("NewRecovery() error = %v", err)
	}
	_, err = runner.Recover(context.Background(), request)
	if !errors.Is(err, migrations.ErrNoDirtyMigration) {
		t.Fatalf("Recover(clean) error = %v, want ErrNoDirtyMigration", err)
	}

	other := mustMigration(t, 1, "create_index", "SELECT 2;\n")
	request, err = migrations.NewRecovery(
		migration.Version(),
		other.Checksum(),
		migrations.RecoveryMarkRolledBack,
	)
	if err != nil {
		t.Fatalf("NewRecovery() error = %v", err)
	}
	_, err = runner.Recover(context.Background(), request)
	if !errors.Is(err, migrations.ErrRecoveryMismatch) {
		t.Fatalf("Recover(checksum) error = %v, want ErrRecoveryMismatch", err)
	}
}

func TestNewRecoveryRejectsInvalidDecision(t *testing.T) {
	t.Parallel()

	_, err := migrations.NewRecovery(0, migrations.Checksum{}, 99)
	if !errors.Is(err, migrations.ErrInvalidRecovery) {
		t.Fatalf("NewRecovery() error = %v, want ErrInvalidRecovery", err)
	}
}
