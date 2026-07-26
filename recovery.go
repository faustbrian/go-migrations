package migrations

import "errors"

// RecoveryAction is an explicit operator assertion about a dirty migration.
type RecoveryAction uint8

const (
	// RecoveryMarkApplied certifies that the migration's full up SQL is present.
	RecoveryMarkApplied RecoveryAction = iota + 1
	// RecoveryMarkRolledBack certifies that all partial effects were removed.
	RecoveryMarkRolledBack
)

var (
	// ErrInvalidRecovery indicates malformed recovery input.
	ErrInvalidRecovery = errors.New("invalid dirty migration recovery")
	// ErrRecoveryMismatch indicates that source identity differs from the
	// operator-reviewed recovery request.
	ErrRecoveryMismatch = errors.New("dirty migration recovery identity mismatch")
	// ErrNoDirtyMigration indicates there is no matching unresolved outcome.
	ErrNoDirtyMigration = errors.New("dirty migration not found")
	// ErrRecoveryUnsupported indicates a backend without recovery conformance.
	ErrRecoveryUnsupported = errors.New("migration backend does not support recovery")
	// ErrRecoveryConflict indicates more than one dirty migration in the ledger.
	ErrRecoveryConflict = errors.New("multiple dirty migrations require recovery")
)

// Recovery is an immutable, checksum-bound operator decision.
type Recovery struct {
	version  Version
	checksum Checksum
	action   RecoveryAction
}

// NewRecovery validates an explicit dirty-state resolution request.
func NewRecovery(version Version, checksum Checksum, action RecoveryAction) (Recovery, error) {
	if version == 0 || checksum == (Checksum{}) ||
		(action != RecoveryMarkApplied && action != RecoveryMarkRolledBack) {
		return Recovery{}, ErrInvalidRecovery
	}

	return Recovery{version: version, checksum: checksum, action: action}, nil
}

// Version returns the reviewed migration version.
func (recovery Recovery) Version() Version { return recovery.version }

// Checksum returns the reviewed source checksum.
func (recovery Recovery) Checksum() Checksum { return recovery.checksum }

// Action returns the operator's verified outcome.
func (recovery Recovery) Action() RecoveryAction { return recovery.action }

// RecoveryResult is the persisted outcome of resolving one dirty migration.
type RecoveryResult struct {
	action RecoveryAction
	record Record
}

// Action returns the applied recovery decision.
func (result RecoveryResult) Action() RecoveryAction { return result.action }

// Record returns the resolved ledger record. MarkRolledBack returns the dirty
// record that was removed; MarkApplied returns its new clean representation.
func (result RecoveryResult) Record() Record { return result.record }
