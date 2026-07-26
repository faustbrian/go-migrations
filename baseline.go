package migrations

import "errors"

var (
	// ErrInvalidBaseline indicates malformed reviewed baseline metadata.
	ErrInvalidBaseline = errors.New("invalid schema baseline")
	// ErrBaselineMismatch indicates live schema drift from the reviewed contract.
	ErrBaselineMismatch = errors.New("schema does not match reviewed baseline")
	// ErrBaselineExists indicates that owned migration history is not empty.
	ErrBaselineExists = errors.New("migration baseline already exists")
	// ErrBaselineVersionConflict indicates Go migrations at or below the
	// baseline cutoff.
	ErrBaselineVersionConflict = errors.New("go migration conflicts with baseline version")
	// ErrBaselineUnsupported indicates a backend without baseline conformance.
	ErrBaselineUnsupported = errors.New("migration backend does not support baselines")
)

// Baseline is an immutable reviewed schema assertion for adopting an existing
// database without replaying historical migrations.
type Baseline struct {
	version     Version
	name        string
	fingerprint Checksum
}

// NewBaseline validates a reviewed baseline contract.
func NewBaseline(version Version, name string, fingerprint Checksum) (Baseline, error) {
	if version == 0 || !migrationNamePattern.MatchString(name) || fingerprint == (Checksum{}) {
		return Baseline{}, ErrInvalidBaseline
	}

	return Baseline{version: version, name: name, fingerprint: fingerprint}, nil
}

// Version returns the immutable cutoff before all Go-owned migrations.
func (baseline Baseline) Version() Version { return baseline.version }

// Name returns the reviewed baseline contract name.
func (baseline Baseline) Name() string { return baseline.name }

// Fingerprint returns the expected canonical schema digest.
func (baseline Baseline) Fingerprint() Checksum { return baseline.fingerprint }
