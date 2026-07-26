package migrations

import (
	"errors"
	"fmt"
	"time"
)

// RecordKind distinguishes an executed migration from an explicit schema
// baseline in the owned ledger.
type RecordKind uint8

const (
	// RecordKindMigration is a migration executed by this package.
	RecordKindMigration RecordKind = iota + 1
	// RecordKindBaseline is a reviewed pre-existing schema assertion.
	RecordKindBaseline
)

// Action identifies the operation represented by a plan step.
type Action uint8

const (
	// ActionApply executes a migration's up SQL.
	ActionApply Action = iota + 1
	// ActionRollback executes a migration's down SQL.
	ActionRollback
)

var (
	// ErrInvalidRecord indicates malformed owned-ledger data.
	ErrInvalidRecord = errors.New("invalid migration record")
	// ErrDirty indicates an interrupted or partially applied migration.
	ErrDirty = errors.New("dirty migration history")
	// ErrChecksumMismatch indicates an applied migration was modified.
	ErrChecksumMismatch = errors.New("migration checksum mismatch")
	// ErrRenamedMigration indicates an applied version now has another name.
	ErrRenamedMigration = errors.New("applied migration was renamed")
	// ErrDeletedMigration indicates an applied migration is absent from source.
	ErrDeletedMigration = errors.New("applied migration was deleted")
	// ErrReorderedHistory indicates non-monotonic or non-prefix history.
	ErrReorderedHistory = errors.New("migration history was reordered")
	// ErrInvalidTarget indicates an impossible or ambiguous plan target.
	ErrInvalidTarget = errors.New("invalid migration target")
	// ErrIrreversible indicates that rollback SQL was not provided.
	ErrIrreversible = errors.New("migration is irreversible")
)

// Record is an immutable entry read from public.go_schema_migrations.
type Record struct {
	kind      RecordKind
	version   Version
	name      string
	checksum  Checksum
	appliedAt time.Time
	duration  time.Duration
	dirty     bool
}

// NewRecord validates data crossing the owned-ledger boundary.
func NewRecord(
	kind RecordKind,
	version Version,
	name string,
	checksum Checksum,
	appliedAt time.Time,
	duration time.Duration,
	dirty bool,
) (Record, error) {
	if kind != RecordKindMigration && kind != RecordKindBaseline {
		return Record{}, ErrInvalidRecord
	}
	if version == 0 || !migrationNamePattern.MatchString(name) {
		return Record{}, ErrInvalidRecord
	}
	if checksum == (Checksum{}) || appliedAt.IsZero() || duration < 0 {
		return Record{}, ErrInvalidRecord
	}

	return Record{
		kind:      kind,
		version:   version,
		name:      name,
		checksum:  checksum,
		appliedAt: appliedAt,
		duration:  duration,
		dirty:     dirty,
	}, nil
}

// Kind returns whether this record is a migration or baseline.
func (record Record) Kind() RecordKind { return record.kind }

// Version returns the record's immutable version.
func (record Record) Version() Version { return record.version }

// Name returns the persisted canonical name.
func (record Record) Name() string { return record.name }

// Checksum returns the persisted migration or schema fingerprint.
func (record Record) Checksum() Checksum { return record.checksum }

// AppliedAt returns when execution or baseline recording completed.
func (record Record) AppliedAt() time.Time { return record.appliedAt }

// Duration returns the measured operation duration.
func (record Record) Duration() time.Duration { return record.duration }

// Dirty reports whether the operation has an unresolved partial outcome.
func (record Record) Dirty() bool { return record.dirty }

// Step is one immutable action in a deterministic plan.
type Step struct {
	action    Action
	migration Migration
}

// Action returns the step operation.
func (step Step) Action() Action { return step.action }

// Migration returns the immutable migration for this step.
func (step Step) Migration() Migration { return step.migration }

// Plan is a deterministic execution plan derived from source and ledger state.
type Plan struct {
	steps []Step
}

// Steps returns a copy so callers cannot mutate a validated plan.
func (plan Plan) Steps() []Step { return append([]Step(nil), plan.steps...) }

// PlanUp validates the complete persisted history and returns pending
// migrations in ascending version order. Any ambiguity fails closed.
func PlanUp(available []Migration, records []Record) (Plan, error) {
	if err := validateAvailableOrder(available); err != nil {
		return Plan{}, err
	}

	baselineVersion, applied, err := validateRecords(records)
	if err != nil {
		return Plan{}, err
	}
	if baselineVersion != 0 {
		for _, migration := range available {
			if migration.Version() <= baselineVersion {
				return Plan{}, fmt.Errorf(
					"%w: migration %d_%s",
					ErrBaselineVersionConflict,
					migration.Version(),
					migration.Name(),
				)
			}
		}
	}

	byVersion := make(map[Version]Migration, len(available))
	for _, migration := range available {
		byVersion[migration.Version()] = migration
	}

	for version, record := range applied {
		migration, exists := byVersion[version]
		if !exists {
			return Plan{}, fmt.Errorf("%w: %d_%s", ErrDeletedMigration, version, record.Name())
		}
		if migration.Name() != record.Name() {
			return Plan{}, fmt.Errorf("%w: version %d", ErrRenamedMigration, version)
		}
		if migration.Checksum() != record.Checksum() {
			return Plan{}, fmt.Errorf("%w: version %d", ErrChecksumMismatch, version)
		}
	}

	steps := make([]Step, 0, len(available)-len(applied))
	seenPending := false
	for _, migration := range available {
		if _, exists := applied[migration.Version()]; exists {
			if seenPending {
				return Plan{}, fmt.Errorf("%w: applied version %d follows a gap", ErrReorderedHistory, migration.Version())
			}
			continue
		}

		seenPending = true
		steps = append(steps, Step{action: ActionApply, migration: migration})
	}

	return Plan{steps: steps}, nil
}

// PlanDown validates the complete persisted history and returns exactly count
// rollback steps in descending version order. It never crosses a baseline.
func PlanDown(available []Migration, records []Record, count uint64) (Plan, error) {
	if count == 0 {
		return Plan{}, ErrInvalidTarget
	}
	if _, err := PlanUp(available, records); err != nil {
		return Plan{}, err
	}

	applied := make(map[Version]Record, len(records))
	for _, record := range records {
		if record.Kind() == RecordKindMigration {
			applied[record.Version()] = record
		}
	}
	if count > uint64(len(applied)) {
		return Plan{}, ErrInvalidTarget
	}

	steps := make([]Step, 0, count)
	for index := len(available) - 1; index >= 0 && uint64(len(steps)) < count; index-- {
		migration := available[index]
		if _, exists := applied[migration.Version()]; !exists {
			continue
		}
		if migration.DownSQL() == "" {
			return Plan{}, fmt.Errorf("%w: version %d", ErrIrreversible, migration.Version())
		}
		steps = append(steps, Step{action: ActionRollback, migration: migration})
	}

	return Plan{steps: steps}, nil
}

func validateAvailableOrder(available []Migration) error {
	var previous Version
	for _, migration := range available {
		if migration.Version() == 0 || migration.Name() == "" || migration.Checksum() == (Checksum{}) {
			return ErrInvalidFormat
		}
		if migration.Version() <= previous {
			return fmt.Errorf("%w: source version %d", ErrReorderedHistory, migration.Version())
		}
		previous = migration.Version()
	}

	return nil
}

func validateRecords(records []Record) (Version, map[Version]Record, error) {
	applied := make(map[Version]Record, len(records))
	var baselineVersion Version
	var previous Version

	for index, record := range records {
		if record.Version() == 0 || record.Name() == "" || record.Checksum() == (Checksum{}) || record.AppliedAt().IsZero() {
			return 0, nil, ErrInvalidRecord
		}
		if record.Dirty() {
			return 0, nil, fmt.Errorf("%w: version %d", ErrDirty, record.Version())
		}

		switch record.Kind() {
		case RecordKindBaseline:
			if index != 0 || baselineVersion != 0 {
				return 0, nil, ErrReorderedHistory
			}
			baselineVersion = record.Version()
			previous = record.Version()
		case RecordKindMigration:
			if record.Version() <= previous {
				return 0, nil, fmt.Errorf("%w: ledger version %d", ErrReorderedHistory, record.Version())
			}
			previous = record.Version()
			applied[record.Version()] = record
		default:
			return 0, nil, ErrInvalidRecord
		}
	}

	return baselineVersion, applied, nil
}
