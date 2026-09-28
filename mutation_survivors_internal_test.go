package migrations

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestNewBaselineAcceptsMaximumLedgerVersion(t *testing.T) {
	t.Parallel()
	checksum := ChecksumData([]byte("reviewed schema"))
	baseline, err := NewBaseline(Version(math.MaxInt64), "reviewed", checksum)
	if err != nil || baseline.Version() != Version(math.MaxInt64) || baseline.Name() != "reviewed" || baseline.Fingerprint() != checksum {
		t.Fatalf("maximum baseline = %#v, %v", baseline, err)
	}
}

func TestNewRecordAcceptsMaximumLedgerVersion(t *testing.T) {
	t.Parallel()
	checksum, applied := ChecksumData([]byte("reviewed schema")), time.Unix(1, 0).UTC()
	for _, kind := range []RecordKind{RecordKindBaseline, RecordKindMigration} {
		record, err := NewRecord(kind, Version(math.MaxInt64), "reviewed", checksum, applied, 0, false)
		if err != nil || record.Version() != Version(math.MaxInt64) || record.Kind() != kind || record.Name() != "reviewed" || record.Checksum() != checksum || !record.AppliedAt().Equal(applied) {
			t.Fatalf("maximum record = %#v, %v", record, err)
		}
	}
}

func TestPlanningAndStatusAcceptCompleteMaximumHistory(t *testing.T) {
	t.Parallel()
	checksum, applied := ChecksumData([]byte("baseline")), time.Unix(1, 0).UTC()
	baseline, err := NewRecord(RecordKindBaseline, 1, "reviewed", checksum, applied, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	records := []Record{baseline}
	available := make([]Migration, 0, MaxMigrationFiles)
	for index := range MaxMigrationFiles {
		migration, err := NewMigration(Version(index+2), "reviewed", TransactionModeDefault, "SELECT 1;", "SELECT 1;")
		if err != nil {
			t.Fatal(err)
		}
		record, err := NewRecord(RecordKindMigration, migration.Version(), migration.Name(), migration.Checksum(), applied, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		available = append(available, migration)
		records = append(records, record)
	}
	plan, err := PlanUp(available, records)
	if err != nil || len(plan.Steps()) != 0 {
		t.Fatalf("complete maximum plan = %#v, %v", plan, err)
	}
	status, err := BuildStatus(available, records)
	if err != nil || len(status.Entries()) != len(records) {
		t.Fatalf("complete maximum status length = %d, %v", len(status.Entries()), err)
	}
	if _, err := PlanUp(append(available, available[0]), records); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("oversized source = %v", err)
	}
	if _, err := PlanUp(available, append(records, records[0])); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("oversized ledger plan = %v", err)
	}
	if _, err := BuildStatus(available, append(records, records[0])); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("oversized ledger status = %v", err)
	}
}

func TestFSSourceRejectsZeroTimeoutBeforeProvider(t *testing.T) {
	t.Parallel()
	source := &FSSource{fs: fixedSourceFileSystem{}, root: "."}
	if _, err := source.Load(context.Background()); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("zero-timeout source = %v, want ErrInvalidSource", err)
	}
}
