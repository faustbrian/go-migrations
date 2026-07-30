package migrations

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestConstructorsRejectEachInvalidFieldIndependently(t *testing.T) {
	t.Parallel()

	checksum := ChecksumData([]byte("valid"))
	baselines := []struct {
		name        string
		version     Version
		baseline    string
		fingerprint Checksum
	}{
		{name: "version", baseline: "valid", fingerprint: checksum},
		{name: "name", version: 1, baseline: "not valid", fingerprint: checksum},
		{name: "fingerprint", version: 1, baseline: "valid"},
	}
	for _, test := range baselines {
		t.Run("baseline "+test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewBaseline(test.version, test.baseline, test.fingerprint); !errors.Is(err, ErrInvalidBaseline) {
				t.Fatalf("NewBaseline() error = %v, want ErrInvalidBaseline", err)
			}
		})
	}

	recoveries := []struct {
		name     string
		version  Version
		checksum Checksum
		action   RecoveryAction
	}{
		{name: "version", checksum: checksum, action: RecoveryMarkApplied},
		{name: "checksum", version: 1, action: RecoveryMarkApplied},
		{name: "action", version: 1, checksum: checksum, action: 99},
	}
	for _, test := range recoveries {
		t.Run("recovery "+test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewRecovery(test.version, test.checksum, test.action); !errors.Is(err, ErrInvalidRecovery) {
				t.Fatalf("NewRecovery() error = %v, want ErrInvalidRecovery", err)
			}
		})
	}
}

func TestMigrationValidationRejectsIndependentEncodingAndSizeFailures(t *testing.T) {
	t.Parallel()

	if _, err := NewMigration(
		1,
		"maximum_size",
		TransactionModeDefault,
		strings.Repeat("x", maximumMigrationFileSize),
		"",
	); err != nil {
		t.Fatalf("NewMigration(maximum size) error = %v", err)
	}

	tests := []struct {
		name    string
		upSQL   string
		downSQL string
		target  error
	}{
		{name: "invalid UTF-8 up", upSQL: string([]byte{0xff}), target: ErrInvalidEncoding},
		{name: "invalid UTF-8 down", upSQL: "SELECT 1;", downSQL: string([]byte{0xff}), target: ErrInvalidEncoding},
		{name: "NUL first in up", upSQL: "\x00SELECT 1;", target: ErrInvalidEncoding},
		{name: "NUL first in down", upSQL: "SELECT 1;", downSQL: "\x00SELECT 2;", target: ErrInvalidEncoding},
		{name: "up exceeds limit", upSQL: strings.Repeat("x", maximumMigrationFileSize+1), target: ErrInvalidEncoding},
		{
			name:    "combined SQL exceeds limit",
			upSQL:   strings.Repeat("x", maximumMigrationFileSize),
			downSQL: "x",
			target:  ErrInvalidEncoding,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewMigration(1, "valid", TransactionModeDefault, test.upSQL, test.downSQL); !errors.Is(err, test.target) {
				t.Fatalf("NewMigration() error = %v, want %v", err, test.target)
			}
		})
	}

	for name, encoded := range map[string]string{
		"malformed hex": "sha256:" + strings.Repeat("g", 64),
		"short digest":  "sha256:01",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseChecksum(encoded); !errors.Is(err, ErrInvalidChecksum) {
				t.Fatalf("ParseChecksum() error = %v, want ErrInvalidChecksum", err)
			}
		})
	}

	if _, err := NewMigration(Version(math.MaxInt64), "valid", TransactionModeDefault, "SELECT 1;", ""); err != nil {
		t.Fatalf("NewMigration(maximum version) error = %v", err)
	}
}

func TestPlannerValidationRejectsEachMalformedSourceField(t *testing.T) {
	t.Parallel()

	valid := internalMigration(t, 1, "valid")
	tests := []struct {
		name   string
		mutate func(Migration) Migration
	}{
		{name: "version", mutate: func(value Migration) Migration { value.version = 0; return value }},
		{name: "name", mutate: func(value Migration) Migration { value.name = ""; return value }},
		{name: "checksum", mutate: func(value Migration) Migration { value.checksum = Checksum{}; return value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateAvailableOrder([]Migration{test.mutate(valid)}); !errors.Is(err, ErrInvalidFormat) {
				t.Fatalf("validateAvailableOrder() error = %v, want ErrInvalidFormat", err)
			}
		})
	}

	if err := validateAvailableOrder([]Migration{
		internalMigration(t, 1, "first"),
		internalMigration(t, 1, "second"),
	}); !errors.Is(err, ErrReorderedHistory) {
		t.Fatalf("validateAvailableOrder(equal versions) error = %v, want ErrReorderedHistory", err)
	}
}

func TestPlanUpRejectsMigrationEqualToBaselineCutoff(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 100, "equal")
	baseline := internalRecord(t, RecordKindBaseline, migration, false)
	if _, err := PlanUp([]Migration{migration}, []Record{baseline}); !errors.Is(err, ErrBaselineVersionConflict) {
		t.Fatalf("PlanUp() error = %v, want ErrBaselineVersionConflict", err)
	}
}

func TestLedgerValidationRejectsEachMalformedRecordField(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 1, "valid")
	valid := internalRecord(t, RecordKindMigration, migration, false)
	tests := []struct {
		name   string
		mutate func(Record) Record
	}{
		{name: "version", mutate: func(value Record) Record { value.version = 0; return value }},
		{name: "name", mutate: func(value Record) Record { value.name = ""; return value }},
		{name: "checksum", mutate: func(value Record) Record { value.checksum = Checksum{}; return value }},
		{name: "applied time", mutate: func(value Record) Record { value.appliedAt = time.Time{}; return value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			record := test.mutate(valid)
			if _, _, err := validateRecords([]Record{record}); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("validateRecords() error = %v, want ErrInvalidRecord", err)
			}
			if _, _, err := statusRecords([]Record{record}); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("statusRecords() error = %v, want ErrInvalidRecord", err)
			}
		})
	}

	first := internalRecord(t, RecordKindMigration, internalMigration(t, 1, "first"), false)
	second := internalRecord(t, RecordKindMigration, internalMigration(t, 1, "second"), false)
	if _, _, err := validateRecords([]Record{first, second}); !errors.Is(err, ErrReorderedHistory) {
		t.Fatalf("validateRecords(equal versions) error = %v, want ErrReorderedHistory", err)
	}
	if _, _, err := statusRecords([]Record{first, second}); !errors.Is(err, ErrReorderedHistory) {
		t.Fatalf("statusRecords(equal versions) error = %v, want ErrReorderedHistory", err)
	}
}

func TestBaselineLedgerPlacementRulesAreIndependent(t *testing.T) {
	t.Parallel()

	baselineMigration := internalMigration(t, 10, "baseline")
	baseline := internalRecord(t, RecordKindBaseline, baselineMigration, false)
	ordinary := internalRecord(t, RecordKindMigration, internalMigration(t, 11, "ordinary"), false)
	dirtyBaseline := baseline
	dirtyBaseline.dirty = true

	tests := []struct {
		name    string
		records []Record
	}{
		{name: "not first", records: []Record{ordinary, baseline}},
		{name: "duplicate", records: []Record{baseline, baseline}},
		{name: "dirty", records: []Record{dirtyBaseline}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := statusRecords(test.records); !errors.Is(err, ErrReorderedHistory) {
				t.Fatalf("statusRecords() error = %v, want ErrReorderedHistory", err)
			}
		})
	}

	if _, _, err := validateRecords([]Record{ordinary, baseline}); !errors.Is(err, ErrReorderedHistory) {
		t.Fatalf("validateRecords(non-first baseline) error = %v, want ErrReorderedHistory", err)
	}
	if _, _, err := validateRecords([]Record{baseline, baseline}); !errors.Is(err, ErrReorderedHistory) {
		t.Fatalf("validateRecords(duplicate baseline) error = %v, want ErrReorderedHistory", err)
	}
}

func TestBackendRecordValidationRejectsEachContractViolation(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 1, "valid")
	valid := internalRecord(t, RecordKindMigration, migration, false)
	tests := []struct {
		name   string
		mutate func(Record) Record
	}{
		{name: "kind", mutate: func(value Record) Record { value.kind = RecordKindBaseline; return value }},
		{name: "version", mutate: func(value Record) Record { value.version++; return value }},
		{name: "name", mutate: func(value Record) Record { value.name += "_other"; return value }},
		{name: "checksum", mutate: func(value Record) Record { value.checksum = ChecksumData([]byte("other")); return value }},
		{name: "applied time", mutate: func(value Record) Record { value.appliedAt = time.Time{}; return value }},
		{name: "dirty", mutate: func(value Record) Record { value.dirty = true; return value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateBackendRecord(migration, test.mutate(valid)); !errors.Is(err, ErrBackendResult) {
				t.Fatalf("validateBackendRecord() error = %v, want ErrBackendResult", err)
			}
		})
	}
}

func TestRunnerRejectsEachMalformedRecoveryAndBaselineField(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 1, "valid")
	runner, err := NewRunner(failureSource{migrations: []Migration{migration}}, &failureBackend{session: &failureSession{}})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	recoveries := []Recovery{
		{checksum: migration.Checksum(), action: RecoveryMarkApplied},
		{version: migration.Version(), action: RecoveryMarkApplied},
		{version: migration.Version(), checksum: migration.Checksum(), action: 99},
	}
	for _, recovery := range recoveries {
		if _, err := runner.Recover(context.Background(), recovery); !errors.Is(err, ErrInvalidRecovery) {
			t.Fatalf("Recover(%#v) error = %v, want ErrInvalidRecovery", recovery, err)
		}
	}

	baselines := []Baseline{
		{name: "valid", fingerprint: migration.Checksum()},
		{version: 1, fingerprint: migration.Checksum()},
		{version: 1, name: "valid"},
	}
	for _, baseline := range baselines {
		if _, err := runner.Baseline(context.Background(), baseline); !errors.Is(err, ErrInvalidBaseline) {
			t.Fatalf("Baseline(%#v) error = %v, want ErrInvalidBaseline", baseline, err)
		}
	}
}

func TestRunnerRejectsEachInvalidRecoveryBackendResult(t *testing.T) {
	t.Parallel()

	migration := internalMigration(t, 2, "target")
	dirty := internalRecord(t, RecordKindMigration, migration, true)
	valid := internalRecord(t, RecordKindMigration, migration, false)
	recovery := Recovery{version: migration.Version(), checksum: migration.Checksum(), action: RecoveryMarkApplied}
	tests := []struct {
		name   string
		mutate func(Record) Record
	}{
		{name: "kind", mutate: func(value Record) Record { value.kind = RecordKindBaseline; return value }},
		{name: "version", mutate: func(value Record) Record { value.version++; return value }},
		{name: "name", mutate: func(value Record) Record { value.name += "_other"; return value }},
		{name: "checksum", mutate: func(value Record) Record { value.checksum = ChecksumData([]byte("other")); return value }},
		{name: "applied time", mutate: func(value Record) Record { value.appliedAt = time.Time{}; return value }},
		{name: "dirty", mutate: func(value Record) Record { value.dirty = true; return value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			session := &capableFailureSession{
				failureSession: &failureSession{records: []Record{dirty}},
				recoveryRecord: test.mutate(valid),
			}
			runner, err := NewRunner(
				failureSource{migrations: []Migration{migration}},
				&failureBackend{session: session},
			)
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}
			if _, err := runner.Recover(context.Background(), recovery); !errors.Is(err, ErrBackendResult) {
				t.Fatalf("Recover() error = %v, want ErrBackendResult", err)
			}
		})
	}
}

func TestRunnerRecoveryScansPastCleanEntries(t *testing.T) {
	t.Parallel()

	first := internalMigration(t, 1, "first")
	target := internalMigration(t, 2, "target")
	third := internalMigration(t, 3, "third")
	targetDirty := internalRecord(t, RecordKindMigration, target, true)
	session := &capableFailureSession{
		failureSession: &failureSession{records: []Record{
			internalRecord(t, RecordKindMigration, first, false),
			targetDirty,
			internalRecord(t, RecordKindMigration, third, false),
		}},
		recoveryRecord: internalRecord(t, RecordKindMigration, target, false),
	}
	runner, err := NewRunner(
		failureSource{migrations: []Migration{first, target, third}},
		&failureBackend{session: session},
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	result, err := runner.Recover(
		context.Background(),
		Recovery{version: target.Version(), checksum: target.Checksum(), action: RecoveryMarkApplied},
	)
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if result.Record().Version() != target.Version() {
		t.Fatalf("Recover().Record().Version() = %d, want %d", result.Record().Version(), target.Version())
	}
}

func TestRunnerRejectsEachInvalidBaselineBackendResult(t *testing.T) {
	t.Parallel()

	baseline := Baseline{version: 100, name: "reviewed", fingerprint: ChecksumData([]byte("schema"))}
	valid := Record{
		kind:      RecordKindBaseline,
		version:   baseline.Version(),
		name:      baseline.Name(),
		checksum:  baseline.Fingerprint(),
		appliedAt: time.Now().UTC(),
	}
	tests := []struct {
		name   string
		mutate func(Record) Record
	}{
		{name: "kind", mutate: func(value Record) Record { value.kind = RecordKindMigration; return value }},
		{name: "version", mutate: func(value Record) Record { value.version++; return value }},
		{name: "name", mutate: func(value Record) Record { value.name += "_other"; return value }},
		{name: "checksum", mutate: func(value Record) Record { value.checksum = ChecksumData([]byte("other")); return value }},
		{name: "applied time", mutate: func(value Record) Record { value.appliedAt = time.Time{}; return value }},
		{name: "dirty", mutate: func(value Record) Record { value.dirty = true; return value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			session := &capableFailureSession{
				failureSession: &failureSession{},
				baselineRecord: test.mutate(valid),
			}
			runner, err := NewRunner(
				failureSource{migrations: []Migration{internalMigration(t, 101, "next")}},
				&failureBackend{session: session},
			)
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}
			if _, err := runner.Baseline(context.Background(), baseline); !errors.Is(err, ErrBackendResult) {
				t.Fatalf("Baseline() error = %v, want ErrBackendResult", err)
			}
		})
	}
}

func TestRunnerBaselineRequiresSourceStrictlyAfterCutoff(t *testing.T) {
	t.Parallel()

	baseline := Baseline{version: 100, name: "reviewed", fingerprint: ChecksumData([]byte("schema"))}
	runner, err := NewRunner(
		failureSource{migrations: []Migration{internalMigration(t, 100, "equal")}},
		&failureBackend{session: &failureSession{}},
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if _, err := runner.Baseline(context.Background(), baseline); !errors.Is(err, ErrBaselineVersionConflict) {
		t.Fatalf("Baseline() error = %v, want ErrBaselineVersionConflict", err)
	}
}

func TestReadMigrationFileRejectsEachHostileBoundary(t *testing.T) {
	t.Parallel()

	valid := strings.Repeat("x", maximumMigrationFileSize)
	source := fstest.MapFS{"migration.sql": &fstest.MapFile{Data: []byte(valid)}}
	contents, err := readMigrationFile(source, "migration.sql")
	if err != nil {
		t.Fatalf("readMigrationFile(maximum size) error = %v", err)
	}
	if len(contents) != maximumMigrationFileSize {
		t.Fatalf("readMigrationFile(maximum size) length = %d, want %d", len(contents), maximumMigrationFileSize)
	}

	tests := []struct {
		name string
		data []byte
	}{
		{name: "oversized", data: []byte(strings.Repeat("x", maximumMigrationFileSize+1))},
		{name: "invalid UTF-8", data: []byte{0xff}},
		{name: "NUL", data: []byte("\x00SELECT 1;")},
		{name: "BOM", data: []byte("\ufeffSELECT 1;")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := fstest.MapFS{"migration.sql": &fstest.MapFile{Data: test.data}}
			if _, err := readMigrationFile(source, "migration.sql"); !errors.Is(err, ErrInvalidEncoding) {
				t.Fatalf("readMigrationFile() error = %v, want ErrInvalidEncoding", err)
			}
		})
	}
}
