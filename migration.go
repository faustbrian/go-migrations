// Package migrations provides engine-neutral database migration contracts.
package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Version is the immutable, monotonically increasing migration identifier.
type Version uint64

// String returns the canonical unpadded decimal representation.
func (version Version) String() string {
	return strconv.FormatUint(uint64(version), 10)
}

// TransactionMode controls whether an engine wraps a migration in a database
// transaction.
type TransactionMode uint8

const (
	// TransactionModeDefault executes the migration atomically.
	TransactionModeDefault TransactionMode = iota
	// TransactionModeNone permits operations PostgreSQL cannot run in a
	// transaction. A failed migration in this mode requires explicit recovery.
	TransactionModeNone
)

var (
	// ErrInvalidVersion indicates that a migration version is zero or malformed.
	ErrInvalidVersion = errors.New("invalid migration version")
	// ErrInvalidName indicates that a migration name is not canonical snake case.
	ErrInvalidName = errors.New("invalid migration name")
	// ErrInvalidTransactionMode indicates an unknown transaction mode.
	ErrInvalidTransactionMode = errors.New("invalid transaction mode")
	// ErrEmptyUpSQL indicates that a migration has no forward operation.
	ErrEmptyUpSQL = errors.New("migration up SQL is empty")
	// ErrInvalidChecksum indicates malformed or unsupported checksum text.
	ErrInvalidChecksum = errors.New("invalid migration checksum")
)

var migrationNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)

// Checksum is the SHA-256 digest of the canonical migration representation.
type Checksum struct {
	sha256 [sha256.Size]byte
}

// ChecksumData computes the stable SHA-256 representation of canonical bytes.
// It is intended for package-defined contracts such as schema fingerprints;
// migration identity uses its stricter built-in canonical representation.
func ChecksumData(canonical []byte) Checksum {
	return Checksum{sha256: sha256.Sum256(canonical)}
}

// ParseChecksum decodes the stable ledger representation of a checksum.
func ParseChecksum(value string) (Checksum, error) {
	const prefix = "sha256:"

	if !strings.HasPrefix(value, prefix) {
		return Checksum{}, ErrInvalidChecksum
	}

	payload := strings.TrimPrefix(value, prefix)
	if payload != strings.ToLower(payload) {
		return Checksum{}, ErrInvalidChecksum
	}
	decoded, err := hex.DecodeString(payload)
	if err != nil || len(decoded) != sha256.Size {
		return Checksum{}, ErrInvalidChecksum
	}

	var digest [sha256.Size]byte
	copy(digest[:], decoded)
	checksum := Checksum{sha256: digest}
	if checksum == (Checksum{}) {
		return Checksum{}, ErrInvalidChecksum
	}

	return checksum, nil
}

// String returns the algorithm-qualified ledger representation.
func (checksum Checksum) String() string {
	return "sha256:" + hex.EncodeToString(checksum.sha256[:])
}

// Migration is an immutable canonical SQL migration.
//
// Fields are intentionally private so callers and execution engines cannot
// mutate identity or persisted checksum state after validation.
type Migration struct {
	version         Version
	name            string
	transactionMode TransactionMode
	upSQL           string
	downSQL         string
	checksum        Checksum
}

// NewMigration validates and constructs a canonical migration.
func NewMigration(
	version Version,
	name string,
	transactionMode TransactionMode,
	upSQL string,
	downSQL string,
) (Migration, error) {
	if version == 0 || version > Version(math.MaxInt64) {
		return Migration{}, ErrInvalidVersion
	}
	if !migrationNamePattern.MatchString(name) {
		return Migration{}, ErrInvalidName
	}
	if transactionMode != TransactionModeDefault && transactionMode != TransactionModeNone {
		return Migration{}, ErrInvalidTransactionMode
	}
	if strings.TrimSpace(upSQL) == "" {
		return Migration{}, ErrEmptyUpSQL
	}
	if downSQL != "" && strings.TrimSpace(downSQL) == "" {
		return Migration{}, ErrInvalidFormat
	}
	if len(upSQL) > maximumMigrationFileSize ||
		len(downSQL) > maximumMigrationFileSize-len(upSQL) ||
		!utf8.ValidString(upSQL) || !utf8.ValidString(downSQL) ||
		strings.IndexByte(upSQL, 0) >= 0 || strings.IndexByte(downSQL, 0) >= 0 {
		return Migration{}, ErrInvalidEncoding
	}

	migration := Migration{
		version:         version,
		name:            name,
		transactionMode: transactionMode,
		upSQL:           upSQL,
		downSQL:         downSQL,
	}
	migration.checksum = checksumMigration(migration)

	return migration, nil
}

// Version returns the immutable migration version.
func (migration Migration) Version() Version { return migration.version }

// Name returns the canonical migration name without version or extension.
func (migration Migration) Name() string { return migration.name }

// TransactionMode returns the execution transaction policy.
func (migration Migration) TransactionMode() TransactionMode {
	return migration.transactionMode
}

// UpSQL returns the canonical forward SQL.
func (migration Migration) UpSQL() string { return migration.upSQL }

// DownSQL returns the canonical rollback SQL, or an empty string when the
// migration is irreversible.
func (migration Migration) DownSQL() string { return migration.downSQL }

// Checksum returns the immutable content digest.
func (migration Migration) Checksum() Checksum { return migration.checksum }

func checksumMigration(migration Migration) Checksum {
	mode := "transaction"
	if migration.transactionMode == TransactionModeNone {
		mode = "none"
	}

	canonical := "go-migrations/v1\nversion:" + strconv.FormatUint(uint64(migration.version), 10) +
		"\nname:" + migration.name +
		"\ntransaction:" + mode +
		"\nup:" + strconv.Itoa(len(migration.upSQL)) + ":" + migration.upSQL +
		"\ndown:" + strconv.Itoa(len(migration.downSQL)) + ":" + migration.downSQL
	digest := sha256.Sum256([]byte(canonical))

	return Checksum{sha256: digest}
}

// GoString prevents checksum internals from becoming an accidental public
// serialization contract while keeping diagnostics useful.
func (checksum Checksum) GoString() string {
	return fmt.Sprintf("Checksum(%q)", checksum.String())
}
