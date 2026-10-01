package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	migrations "github.com/faustbrian/go-migrations/v3"
)

func TestRunnerBaselineRecordsReviewedFingerprintWithoutReplayingMigrations(t *testing.T) {
	t.Parallel()

	fingerprint, err := migrations.ParseChecksum("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("ParseChecksum() error = %v", err)
	}
	baseline, err := migrations.NewBaseline(100, "laravel_production_v1", fingerprint)
	if err != nil {
		t.Fatalf("NewBaseline() error = %v", err)
	}
	firstGoMigration := mustMigration(t, 101, "add_go_table", "SELECT 1;\n")
	backend := &recordingBackend{baselineFingerprint: fingerprint}
	runner, err := migrations.NewRunner(
		staticSource{migrations: []migrations.Migration{firstGoMigration}},
		backend,
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	record, err := runner.Baseline(context.Background(), baseline)
	if err != nil {
		t.Fatalf("Baseline() error = %v", err)
	}
	if record.Kind() != migrations.RecordKindBaseline || record.Version() != 100 {
		t.Fatalf("Baseline() record = %#v", record)
	}
	if got := backend.calls(); !equalStrings(got, []string{
		"acquire",
		"prepare",
		"records",
		"baseline:100",
		"release",
	}) {
		t.Fatalf("backend calls = %v", got)
	}
}

func TestRunnerBaselineFailsClosedForExistingOrPreBaselineGoHistory(t *testing.T) {
	t.Parallel()

	fingerprint, err := migrations.ParseChecksum("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("ParseChecksum() error = %v", err)
	}
	baseline, err := migrations.NewBaseline(100, "laravel_production_v1", fingerprint)
	if err != nil {
		t.Fatalf("NewBaseline() error = %v", err)
	}
	legacy := mustMigration(t, 99, "legacy_go_migration", "SELECT 1;\n")

	backend := &recordingBackend{baselineFingerprint: fingerprint}
	runner, err := migrations.NewRunner(staticSource{migrations: []migrations.Migration{legacy}}, backend)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	_, err = runner.Baseline(context.Background(), baseline)
	if !errors.Is(err, migrations.ErrBaselineVersionConflict) {
		t.Fatalf("Baseline(old source) error = %v, want ErrBaselineVersionConflict", err)
	}

	existingBackend := &recordingBackend{
		baselineFingerprint: fingerprint,
		records: []migrations.Record{
			mustRecord(t, migrations.RecordKindMigration, legacy, false),
		},
	}
	runner, err = migrations.NewRunner(staticSource{}, existingBackend)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	_, err = runner.Baseline(context.Background(), baseline)
	if !errors.Is(err, migrations.ErrBaselineExists) {
		t.Fatalf("Baseline(existing) error = %v, want ErrBaselineExists", err)
	}
}

func TestNewBaselineRejectsInvalidContract(t *testing.T) {
	t.Parallel()

	if _, err := migrations.NewBaseline(0, "laravel", migrations.Checksum{}); !errors.Is(err, migrations.ErrInvalidBaseline) {
		t.Fatalf("NewBaseline() error = %v, want ErrInvalidBaseline", err)
	}
}

func TestNewBaselineBoundsCanonicalNameBytes(t *testing.T) {
	t.Parallel()

	fingerprint, err := migrations.ParseChecksum("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("ParseChecksum() error = %v", err)
	}
	maximumName := strings.Repeat("a", migrations.MaxMigrationNameBytes)
	if _, err := migrations.NewBaseline(1, maximumName, fingerprint); err != nil {
		t.Fatalf("NewBaseline(maximum name) error = %v", err)
	}
	if _, err := migrations.NewBaseline(1, maximumName+"a", fingerprint); !errors.Is(err, migrations.ErrInvalidBaseline) {
		t.Fatalf("NewBaseline(oversized name) error = %v, want ErrInvalidBaseline", err)
	}
}

func mustBaselineRecord(t *testing.T, version migrations.Version) migrations.Record {
	t.Helper()

	fingerprint, err := migrations.ParseChecksum("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("ParseChecksum() error = %v", err)
	}
	baseline, err := migrations.NewBaseline(version, "laravel_production_v1", fingerprint)
	if err != nil {
		t.Fatalf("NewBaseline() error = %v", err)
	}
	backend := &recordingBackend{baselineFingerprint: fingerprint}
	runner, err := migrations.NewRunner(staticSource{}, backend)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	record, err := runner.Baseline(context.Background(), baseline)
	if err != nil {
		t.Fatalf("Baseline() error = %v", err)
	}

	return record
}
