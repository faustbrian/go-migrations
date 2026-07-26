package migrations_test

import (
	"context"
	"testing"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func TestBuildStatusReportsBaselineAppliedDirtyAndPendingState(t *testing.T) {
	t.Parallel()

	first := mustMigration(t, 101, "create_users", "SELECT 1;\n")
	second := mustMigration(t, 102, "add_email", "SELECT 2;\n")
	third := mustMigration(t, 103, "add_index", "SELECT 3;\n")
	baseline := mustBaselineRecord(t, 100)
	records := []migrations.Record{
		baseline,
		mustRecord(t, migrations.RecordKindMigration, first, false),
		mustRecord(t, migrations.RecordKindMigration, second, true),
	}

	status, err := migrations.BuildStatus(
		[]migrations.Migration{first, second, third},
		records,
	)
	if err != nil {
		t.Fatalf("BuildStatus() error = %v", err)
	}
	entries := status.Entries()
	if len(entries) != 4 {
		t.Fatalf("Entries() count = %d, want 4", len(entries))
	}
	wantStates := []migrations.State{
		migrations.StateBaseline,
		migrations.StateApplied,
		migrations.StateDirty,
		migrations.StatePending,
	}
	for index, want := range wantStates {
		if entries[index].State() != want {
			t.Fatalf("Entries()[%d].State() = %v, want %v", index, entries[index].State(), want)
		}
	}
	if entries[0].Version() != 100 || entries[3].Version() != 103 {
		t.Fatalf("Entries() versions = %#v", entries)
	}
	if entries[2].Checksum() != second.Checksum() || entries[2].Name() != second.Name() {
		t.Fatalf("dirty entry = %#v", entries[2])
	}
	entries[0] = migrations.StatusEntry{}
	if status.Entries()[0].State() != migrations.StateBaseline {
		t.Fatal("Entries() exposed mutable status storage")
	}
}

func TestRunnerStatusAndPlanUseLockedLedgerSnapshot(t *testing.T) {
	t.Parallel()

	first := mustMigration(t, 1, "create_users", "SELECT 1;\n")
	second := mustMigration(t, 2, "add_email", "SELECT 2;\n")
	backend := &recordingBackend{records: []migrations.Record{
		mustRecord(t, migrations.RecordKindMigration, first, false),
	}}
	runner, err := migrations.NewRunner(
		staticSource{migrations: []migrations.Migration{first, second}},
		backend,
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	status, err := runner.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(status.Entries()) != 2 || status.Entries()[1].State() != migrations.StatePending {
		t.Fatalf("Status().Entries() = %#v", status.Entries())
	}
	if got := backend.calls(); !equalStrings(got, []string{
		"acquire",
		"prepare",
		"records",
		"release",
	}) {
		t.Fatalf("Status() backend calls = %v", got)
	}

	backend.clearCalls()
	plan, err := runner.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(plan.Steps()) != 1 || plan.Steps()[0].Migration().Version() != 2 {
		t.Fatalf("Plan().Steps() = %#v", plan.Steps())
	}
	if got := backend.calls(); !equalStrings(got, []string{
		"acquire",
		"prepare",
		"records",
		"release",
	}) {
		t.Fatalf("Plan() backend calls = %v", got)
	}
}
