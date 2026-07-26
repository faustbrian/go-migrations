package migrations_test

import (
	"errors"
	"testing"
	"time"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func TestPlanUpReturnsOnlyPendingMigrations(t *testing.T) {
	t.Parallel()

	first := mustMigration(t, 1, "create_users", "CREATE TABLE users (id bigint);\n")
	second := mustMigration(t, 2, "add_email", "ALTER TABLE users ADD email text;\n")
	record := mustRecord(t, migrations.RecordKindMigration, first, false)

	plan, err := migrations.PlanUp([]migrations.Migration{first, second}, []migrations.Record{record})
	if err != nil {
		t.Fatalf("PlanUp() error = %v", err)
	}
	steps := plan.Steps()
	if len(steps) != 1 {
		t.Fatalf("Steps() count = %d, want 1", len(steps))
	}
	if steps[0].Action() != migrations.ActionApply || steps[0].Migration().Version() != 2 {
		t.Fatalf("Steps()[0] = %#v", steps[0])
	}
	steps[0] = migrations.Step{}
	if plan.Steps()[0].Migration().Version() != 2 {
		t.Fatal("Steps() exposed mutable plan storage")
	}
}

func TestPlanUpFailsClosedOnDivergentHistory(t *testing.T) {
	t.Parallel()

	first := mustMigration(t, 1, "create_users", "CREATE TABLE users (id bigint);\n")
	second := mustMigration(t, 2, "add_email", "ALTER TABLE users ADD email text;\n")
	mutated := mustMigration(t, 1, "create_users", "CREATE TABLE users (id uuid);\n")
	renamed := mustMigration(t, 1, "create_accounts", first.UpSQL())

	tests := []struct {
		name        string
		available   []migrations.Migration
		records     []migrations.Record
		targetError error
	}{
		{
			name:        "dirty record",
			available:   []migrations.Migration{first},
			records:     []migrations.Record{mustRecord(t, migrations.RecordKindMigration, first, true)},
			targetError: migrations.ErrDirty,
		},
		{
			name:        "checksum mutation",
			available:   []migrations.Migration{mutated},
			records:     []migrations.Record{mustRecord(t, migrations.RecordKindMigration, first, false)},
			targetError: migrations.ErrChecksumMismatch,
		},
		{
			name:        "renamed migration",
			available:   []migrations.Migration{renamed},
			records:     []migrations.Record{mustRecord(t, migrations.RecordKindMigration, first, false)},
			targetError: migrations.ErrRenamedMigration,
		},
		{
			name:        "deleted migration",
			available:   []migrations.Migration{second},
			records:     []migrations.Record{mustRecord(t, migrations.RecordKindMigration, first, false)},
			targetError: migrations.ErrDeletedMigration,
		},
		{
			name:      "reordered ledger",
			available: []migrations.Migration{first, second},
			records: []migrations.Record{
				mustRecord(t, migrations.RecordKindMigration, second, false),
				mustRecord(t, migrations.RecordKindMigration, first, false),
			},
			targetError: migrations.ErrReorderedHistory,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := migrations.PlanUp(test.available, test.records)
			if !errors.Is(err, test.targetError) {
				t.Fatalf("PlanUp() error = %v, want %v", err, test.targetError)
			}
		})
	}
}

func TestPlanUpStartsStrictlyAfterBaseline(t *testing.T) {
	t.Parallel()

	fingerprint, err := migrations.ParseChecksum("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("ParseChecksum() error = %v", err)
	}
	baseline, err := migrations.NewRecord(
		migrations.RecordKindBaseline,
		100,
		"laravel_production_v1",
		fingerprint,
		time.Unix(1_700_000_000, 0).UTC(),
		0,
		false,
	)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}
	firstGo := mustMigration(t, 101, "add_go_owned_table", "SELECT 2;\n")

	plan, err := migrations.PlanUp(
		[]migrations.Migration{firstGo},
		[]migrations.Record{baseline},
	)
	if err != nil {
		t.Fatalf("PlanUp() error = %v", err)
	}
	if len(plan.Steps()) != 1 || plan.Steps()[0].Migration().Version() != 101 {
		t.Fatalf("Steps() = %#v, want only version 101", plan.Steps())
	}
}

func TestPlanUpRejectsGoMigrationAtOrBeforeBaseline(t *testing.T) {
	t.Parallel()

	baseline := mustBaselineRecord(t, 100)
	legacy := mustMigration(t, 99, "legacy_schema", "SELECT 1;\n")

	_, err := migrations.PlanUp(
		[]migrations.Migration{legacy},
		[]migrations.Record{baseline},
	)
	if !errors.Is(err, migrations.ErrBaselineVersionConflict) {
		t.Fatalf("PlanUp() error = %v, want ErrBaselineVersionConflict", err)
	}
}

func TestPlanDownRollsBackNewestAppliedMigrationFirst(t *testing.T) {
	t.Parallel()

	first := mustReversibleMigration(t, 1, "create_users")
	second := mustReversibleMigration(t, 2, "add_email")
	records := []migrations.Record{
		mustRecord(t, migrations.RecordKindMigration, first, false),
		mustRecord(t, migrations.RecordKindMigration, second, false),
	}

	plan, err := migrations.PlanDown(
		[]migrations.Migration{first, second},
		records,
		2,
	)
	if err != nil {
		t.Fatalf("PlanDown() error = %v", err)
	}
	steps := plan.Steps()
	if len(steps) != 2 ||
		steps[0].Action() != migrations.ActionRollback ||
		steps[0].Migration().Version() != 2 ||
		steps[1].Migration().Version() != 1 {
		t.Fatalf("Steps() = %#v", steps)
	}
}

func TestPlanDownFailsClosedForIrreversibleOrExcessiveRollback(t *testing.T) {
	t.Parallel()

	irreversible := mustMigration(t, 1, "create_users", "SELECT 1;\n")
	reversible := mustReversibleMigration(t, 1, "create_users")

	_, err := migrations.PlanDown(
		[]migrations.Migration{irreversible},
		[]migrations.Record{mustRecord(t, migrations.RecordKindMigration, irreversible, false)},
		1,
	)
	if !errors.Is(err, migrations.ErrIrreversible) {
		t.Fatalf("PlanDown(irreversible) error = %v, want ErrIrreversible", err)
	}

	_, err = migrations.PlanDown(
		[]migrations.Migration{reversible},
		[]migrations.Record{mustRecord(t, migrations.RecordKindMigration, reversible, false)},
		2,
	)
	if !errors.Is(err, migrations.ErrInvalidTarget) {
		t.Fatalf("PlanDown(excessive) error = %v, want ErrInvalidTarget", err)
	}

	_, err = migrations.PlanDown(nil, nil, 0)
	if !errors.Is(err, migrations.ErrInvalidTarget) {
		t.Fatalf("PlanDown(zero) error = %v, want ErrInvalidTarget", err)
	}
}

func mustMigration(t *testing.T, version migrations.Version, name string, upSQL string) migrations.Migration {
	t.Helper()

	migration, err := migrations.NewMigration(
		version,
		name,
		migrations.TransactionModeDefault,
		upSQL,
		"",
	)
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}

	return migration
}

func mustReversibleMigration(
	t *testing.T,
	version migrations.Version,
	name string,
) migrations.Migration {
	t.Helper()

	migration, err := migrations.NewMigration(
		version,
		name,
		migrations.TransactionModeDefault,
		"SELECT 1;\n",
		"SELECT 2;\n",
	)
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}

	return migration
}

func mustRecord(
	t *testing.T,
	kind migrations.RecordKind,
	migration migrations.Migration,
	dirty bool,
) migrations.Record {
	t.Helper()

	record, err := migrations.NewRecord(
		kind,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		time.Unix(1_700_000_000, 0).UTC(),
		time.Second,
		dirty,
	)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	return record
}
