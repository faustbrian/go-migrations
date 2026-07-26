package migrations

import (
	"fmt"
	"time"
)

// State is the deterministic relationship between source and owned ledger.
type State uint8

const (
	// StateBaseline is the reviewed adoption boundary for an existing schema.
	StateBaseline State = iota + 1
	// StateApplied is a clean, checksum-matched applied migration.
	StateApplied
	// StateDirty is an explicitly unresolved partial or uncertain outcome.
	StateDirty
	// StatePending is a source migration not yet represented in the ledger.
	StatePending
)

// StatusEntry is one immutable source or ledger status item.
type StatusEntry struct {
	state     State
	version   Version
	name      string
	checksum  Checksum
	appliedAt time.Time
	duration  time.Duration
}

// State returns the item's deterministic state.
func (entry StatusEntry) State() State { return entry.state }

// Version returns the migration or baseline version.
func (entry StatusEntry) Version() Version { return entry.version }

// Name returns the canonical migration or baseline name.
func (entry StatusEntry) Name() string { return entry.name }

// Checksum returns the source checksum or persisted baseline fingerprint.
func (entry StatusEntry) Checksum() Checksum { return entry.checksum }

// AppliedAt returns the persisted completion time, or zero for pending work.
func (entry StatusEntry) AppliedAt() time.Time { return entry.appliedAt }

// Duration returns the persisted execution duration, or zero for pending work.
func (entry StatusEntry) Duration() time.Duration { return entry.duration }

// Status is an immutable, version-ordered snapshot.
type Status struct {
	entries []StatusEntry
}

// Entries returns a copy so callers cannot mutate the validated snapshot.
func (status Status) Entries() []StatusEntry {
	return append([]StatusEntry(nil), status.entries...)
}

// BuildStatus validates source and persisted identity while preserving dirty
// entries as explicit status instead of hiding them behind a generic error.
func BuildStatus(available []Migration, records []Record) (Status, error) {
	if err := validateAvailableOrder(available); err != nil {
		return Status{}, err
	}

	baseline, applied, err := statusRecords(records)
	if err != nil {
		return Status{}, err
	}
	byVersion := make(map[Version]Migration, len(available))
	for _, migration := range available {
		if baseline.Version() != 0 && migration.Version() <= baseline.Version() {
			return Status{}, fmt.Errorf(
				"%w: migration %d_%s",
				ErrBaselineVersionConflict,
				migration.Version(),
				migration.Name(),
			)
		}
		byVersion[migration.Version()] = migration
	}

	for version, record := range applied {
		migration, exists := byVersion[version]
		if !exists {
			return Status{}, fmt.Errorf("%w: %d_%s", ErrDeletedMigration, version, record.Name())
		}
		if migration.Name() != record.Name() {
			return Status{}, fmt.Errorf("%w: version %d", ErrRenamedMigration, version)
		}
		if migration.Checksum() != record.Checksum() {
			return Status{}, fmt.Errorf("%w: version %d", ErrChecksumMismatch, version)
		}
	}

	entries := make([]StatusEntry, 0, len(available)+1)
	if baseline.Version() != 0 {
		entries = append(entries, statusEntryFromRecord(StateBaseline, baseline))
	}
	seenPending := false
	for _, migration := range available {
		record, exists := applied[migration.Version()]
		if !exists {
			seenPending = true
			entries = append(entries, StatusEntry{
				state:    StatePending,
				version:  migration.Version(),
				name:     migration.Name(),
				checksum: migration.Checksum(),
			})
			continue
		}
		if seenPending {
			return Status{}, fmt.Errorf(
				"%w: applied version %d follows a gap",
				ErrReorderedHistory,
				migration.Version(),
			)
		}
		state := StateApplied
		if record.Dirty() {
			state = StateDirty
		}
		entries = append(entries, statusEntryFromRecord(state, record))
	}

	return Status{entries: entries}, nil
}

func statusEntryFromRecord(state State, record Record) StatusEntry {
	return StatusEntry{
		state:     state,
		version:   record.Version(),
		name:      record.Name(),
		checksum:  record.Checksum(),
		appliedAt: record.AppliedAt(),
		duration:  record.Duration(),
	}
}

func statusRecords(records []Record) (Record, map[Version]Record, error) {
	applied := make(map[Version]Record, len(records))
	var baseline Record
	var previous Version

	for index, record := range records {
		if record.Version() == 0 || record.Name() == "" ||
			record.Checksum() == (Checksum{}) || record.AppliedAt().IsZero() {
			return Record{}, nil, ErrInvalidRecord
		}
		switch record.Kind() {
		case RecordKindBaseline:
			if index != 0 || baseline.Version() != 0 || record.Dirty() {
				return Record{}, nil, ErrReorderedHistory
			}
			baseline = record
			previous = record.Version()
		case RecordKindMigration:
			if record.Version() <= previous {
				return Record{}, nil, fmt.Errorf(
					"%w: ledger version %d",
					ErrReorderedHistory,
					record.Version(),
				)
			}
			previous = record.Version()
			applied[record.Version()] = record
		default:
			return Record{}, nil, ErrInvalidRecord
		}
	}

	return baseline, applied, nil
}
