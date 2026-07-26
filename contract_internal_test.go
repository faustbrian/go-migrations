package migrations

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestChecksumContractIncludesCanonicalDiagnostics(t *testing.T) {
	t.Parallel()

	checksum := ChecksumData([]byte("reviewed schema"))
	if checksum == (Checksum{}) {
		t.Fatal("ChecksumData() returned zero checksum")
	}
	if got, want := fmt.Sprintf("%#v", checksum), fmt.Sprintf("Checksum(%q)", checksum.String()); got != want {
		t.Fatalf("checksum diagnostic = %q, want %q", got, want)
	}
}

func TestRecordRejectsEveryMalformedLedgerField(t *testing.T) {
	t.Parallel()

	checksum := ChecksumData([]byte("migration"))
	now := time.Now().UTC()
	tests := []struct {
		name       string
		kind       RecordKind
		version    Version
		recordName string
		checksum   Checksum
		appliedAt  time.Time
		duration   time.Duration
	}{
		{name: "kind", kind: 99, version: 1, recordName: "valid", checksum: checksum, appliedAt: now},
		{name: "version", kind: RecordKindMigration, recordName: "valid", checksum: checksum, appliedAt: now},
		{name: "name", kind: RecordKindMigration, version: 1, recordName: "Not Valid", checksum: checksum, appliedAt: now},
		{name: "checksum", kind: RecordKindMigration, version: 1, recordName: "valid", appliedAt: now},
		{name: "applied at", kind: RecordKindMigration, version: 1, recordName: "valid", checksum: checksum},
		{name: "duration", kind: RecordKindMigration, version: 1, recordName: "valid", checksum: checksum, appliedAt: now, duration: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRecord(
				test.kind,
				test.version,
				test.recordName,
				test.checksum,
				test.appliedAt,
				test.duration,
				false,
			)
			if !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("NewRecord() error = %v, want ErrInvalidRecord", err)
			}
		})
	}

	record, err := NewRecord(RecordKindMigration, 1, "valid", checksum, now, time.Second, true)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}
	if record.Kind() != RecordKindMigration || record.Version() != 1 ||
		record.Name() != "valid" || record.Checksum() != checksum ||
		record.AppliedAt() != now || record.Duration() != time.Second || !record.Dirty() {
		t.Fatalf("record accessors returned inconsistent values: %#v", record)
	}
}

func TestPlannerRejectsMalformedInMemoryContracts(t *testing.T) {
	t.Parallel()

	valid := internalMigration(t, 1, "valid")
	now := time.Now().UTC()
	tests := []struct {
		name      string
		available []Migration
		records   []Record
		target    error
	}{
		{name: "zero source value", available: []Migration{{}}, target: ErrInvalidFormat},
		{name: "duplicate source version", available: []Migration{valid, valid}, target: ErrReorderedHistory},
		{name: "zero ledger value", available: []Migration{valid}, records: []Record{{}}, target: ErrInvalidRecord},
		{
			name:      "unknown ledger kind",
			available: []Migration{valid},
			records: []Record{{
				kind: 99, version: 1, name: "valid", checksum: valid.Checksum(), appliedAt: now,
			}},
			target: ErrInvalidRecord,
		},
		{
			name:      "baseline after migration",
			available: []Migration{},
			records: []Record{
				internalRecord(t, RecordKindMigration, valid, false),
				{kind: RecordKindBaseline, version: 2, name: "baseline", checksum: valid.Checksum(), appliedAt: now},
			},
			target: ErrReorderedHistory,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := PlanUp(test.available, test.records)
			if !errors.Is(err, test.target) {
				t.Fatalf("PlanUp() error = %v, want %v", err, test.target)
			}
		})
	}

	if _, err := PlanDown([]Migration{valid}, nil, 0); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("PlanDown(0) error = %v, want ErrInvalidTarget", err)
	}
}

func TestStatusRejectsEveryDivergentHistoryShape(t *testing.T) {
	t.Parallel()

	first := internalMigration(t, 1, "first")
	second := internalMigration(t, 2, "second")
	mutated := internalMigration(t, 1, "first_mutated")
	now := time.Now().UTC()
	tests := []struct {
		name      string
		available []Migration
		records   []Record
		target    error
	}{
		{name: "invalid source", available: []Migration{{}}, target: ErrInvalidFormat},
		{name: "invalid record", available: []Migration{first}, records: []Record{{}}, target: ErrInvalidRecord},
		{name: "unknown kind", available: []Migration{first}, records: []Record{{kind: 99, version: 1, name: "first", checksum: first.Checksum(), appliedAt: now}}, target: ErrInvalidRecord},
		{name: "dirty baseline", records: []Record{{kind: RecordKindBaseline, version: 1, name: "baseline", checksum: first.Checksum(), appliedAt: now, dirty: true}}, target: ErrReorderedHistory},
		{name: "deleted", records: []Record{internalRecord(t, RecordKindMigration, first, false)}, target: ErrDeletedMigration},
		{name: "renamed", available: []Migration{mutated}, records: []Record{internalRecord(t, RecordKindMigration, first, false)}, target: ErrRenamedMigration},
		{
			name:      "checksum",
			available: []Migration{internalMigrationWithSQL(t, 1, "first", "SELECT 2;")},
			records:   []Record{internalRecord(t, RecordKindMigration, first, false)},
			target:    ErrChecksumMismatch,
		},
		{
			name:      "gap",
			available: []Migration{first, second},
			records:   []Record{internalRecord(t, RecordKindMigration, second, false)},
			target:    ErrReorderedHistory,
		},
		{
			name:      "baseline conflict",
			available: []Migration{first},
			records:   []Record{{kind: RecordKindBaseline, version: 1, name: "baseline", checksum: first.Checksum(), appliedAt: now}},
			target:    ErrBaselineVersionConflict,
		},
		{
			name:      "ledger order",
			available: []Migration{first, second},
			records: []Record{
				internalRecord(t, RecordKindMigration, second, false),
				internalRecord(t, RecordKindMigration, first, false),
			},
			target: ErrReorderedHistory,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildStatus(test.available, test.records)
			if !errors.Is(err, test.target) {
				t.Fatalf("BuildStatus() error = %v, want %v", err, test.target)
			}
		})
	}
}

func TestPlanCoversGapValidationAndPendingRollbackScan(t *testing.T) {
	t.Parallel()

	first := internalMigration(t, 1, "first")
	second := internalMigration(t, 2, "second")
	third := internalMigration(t, 3, "third")
	if _, err := PlanUp(
		[]Migration{first, second},
		[]Record{internalRecord(t, RecordKindMigration, second, false)},
	); !errors.Is(err, ErrReorderedHistory) {
		t.Fatalf("PlanUp(gap) error = %v", err)
	}
	if _, err := PlanDown([]Migration{{}}, nil, 1); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("PlanDown(invalid) error = %v", err)
	}
	plan, err := PlanDown(
		[]Migration{first, second, third},
		[]Record{
			internalRecord(t, RecordKindMigration, first, false),
			internalRecord(t, RecordKindMigration, second, false),
		},
		1,
	)
	if err != nil {
		t.Fatalf("PlanDown(pending newest) error = %v", err)
	}
	if len(plan.Steps()) != 1 || plan.Steps()[0].Migration().Version() != 2 {
		t.Fatalf("PlanDown(pending newest) = %#v", plan.Steps())
	}
}

func TestStatusEntryTimeAccessors(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 1, "timed")
	record, err := NewRecord(
		RecordKindMigration,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		time.Now().UTC(),
		2*time.Second,
		false,
	)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}
	status, err := BuildStatus([]Migration{migration}, []Record{record})
	if err != nil {
		t.Fatalf("BuildStatus() error = %v", err)
	}
	entry := status.Entries()[0]
	if entry.AppliedAt() != record.AppliedAt() || entry.Duration() != record.Duration() {
		t.Fatalf("status times = %v, %v", entry.AppliedAt(), entry.Duration())
	}
}

func internalMigration(t *testing.T, version Version, name string) Migration {
	t.Helper()

	return internalMigrationWithSQL(t, version, name, "SELECT 1;")
}

func internalMigrationWithSQL(t *testing.T, version Version, name string, sql string) Migration {
	t.Helper()

	migration, err := NewMigration(version, name, TransactionModeDefault, sql, "SELECT 1;")
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}

	return migration
}

func internalRecord(t *testing.T, kind RecordKind, migration Migration, dirty bool) Record {
	t.Helper()

	record, err := NewRecord(
		kind,
		migration.Version(),
		migration.Name(),
		migration.Checksum(),
		time.Now().UTC(),
		0,
		dirty,
	)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	return record
}
