// Package migrations loads immutable SQL histories, validates checksum-bound
// ledger state, and coordinates plans, execution, baselines and dirty recovery.
// Construct a Source and Backend, then use NewRunner for database operations;
// PlanUp, PlanDown and BuildStatus are database-independent validation helpers.
//
// Each runner operation loads source before acquiring a backend session and
// validates that loaded history against the locked ledger. Callers must keep
// deployment sources immutable; the database lock does not freeze a filesystem.
// The runner borrows its dependencies, starts no goroutines and does not close
// the caller's database. Shared sources, backends and observers must support
// the concurrency with which callers invoke them.
//
// SQL is trusted deployment code, not a sandboxed or authorized request. A
// checksum detects identity changes; it is neither a signature nor proof that
// SQL is safe or reversible. Recovery records an operator-reviewed assertion,
// not an automatic inspection or repair of database effects. See the owned v1
// format in docs/migration-format.md and lifecycle in docs/operations.md.
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

// Version is a numeric migration identity. NewMigration, NewBaseline and
// NewRecord accept only 1 through the maximum signed 64-bit integer; source and
// ledger histories must be strictly increasing, not necessarily contiguous.
type Version uint64

// String returns unpadded decimal text without validating the version.
func (version Version) String() string {
	return strconv.FormatUint(uint64(version), 10)
}

// TransactionMode controls whether an engine wraps a migration in a database
// transaction.
type TransactionMode uint8

const (
	// TransactionModeDefault requests one transaction for SQL and ledger writes.
	TransactionModeDefault TransactionMode = iota
	// TransactionModeNone permits operations PostgreSQL cannot run in a
	// transaction. An unresolved dirty outcome requires explicit recovery.
	TransactionModeNone
)

// MaxMigrationNameBytes is the largest canonical migration, baseline, or
// ledger-record name accepted by the public contracts.
const MaxMigrationNameBytes = 255

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

// Checksum is a SHA-256 digest used for migration identity or a package-defined
// schema fingerprint. Its zero value is invalid for persisted contracts.
type Checksum struct {
	sha256 [sha256.Size]byte
}

// ChecksumData hashes the supplied bytes with SHA-256 without normalization,
// validation or authentication. Callers define any canonical framing; migration
// identity instead uses the built-in versioned, byte-length-prefixed format.
func ChecksumData(canonical []byte) Checksum {
	return Checksum{sha256: sha256.Sum256(canonical)}
}

// ParseChecksum accepts exactly sha256: followed by 64 lowercase hexadecimal
// digits, rejecting the all-zero digest. Other spellings are not normalized.
func ParseChecksum(value string) (Checksum, error) {
	const prefix = "sha256:"

	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return Checksum{}, ErrInvalidChecksum
	}

	payload := strings.TrimPrefix(value, prefix)
	if payload != strings.ToLower(payload) {
		return Checksum{}, ErrInvalidChecksum
	}
	decoded, err := hex.DecodeString(payload)
	// The admitted payload has exactly 2*sha256.Size bytes, so a successful
	// hexadecimal decode necessarily yields exactly sha256.Size bytes.
	if err != nil {
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

// NewMigration validates identity, transaction mode and UTF-8, NUL-free SQL
// totaling at most 16 MiB. Names contain lowercase ASCII letters/digits with
// single separating underscores and at most MaxMigrationNameBytes bytes.
// Up SQL must not be blank; empty down SQL denotes irreversibility, whereas
// nonempty whitespace-only down SQL is rejected. SQL bytes and line endings are
// preserved, not parsed for grammar or normalized before the v1 checksum.
// Unlike FSSource file validation, this constructor does not reject a BOM.
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
	if !validMigrationName(name) {
		return Migration{}, ErrInvalidName
	}
	if transactionMode != TransactionModeDefault && transactionMode != TransactionModeNone {
		return Migration{}, ErrInvalidTransactionMode
	}
	if len(upSQL) > maximumMigrationFileSize ||
		len(downSQL) > maximumMigrationFileSize-len(upSQL) {
		return Migration{}, ErrInvalidEncoding
	}
	if strings.TrimSpace(upSQL) == "" {
		return Migration{}, ErrEmptyUpSQL
	}
	if downSQL != "" && strings.TrimSpace(downSQL) == "" {
		return Migration{}, ErrInvalidFormat
	}
	if !utf8.ValidString(upSQL) || !utf8.ValidString(downSQL) ||
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

func validMigrationName(name string) bool {
	return len(name) <= MaxMigrationNameBytes && migrationNamePattern.MatchString(name)
}

// Version returns the immutable migration version.
func (migration Migration) Version() Version { return migration.version }

// Name returns the canonical migration name without version or extension.
func (migration Migration) Name() string { return migration.name }

// TransactionMode returns the execution transaction policy.
func (migration Migration) TransactionMode() TransactionMode {
	return migration.transactionMode
}

// UpSQL returns the exact validated forward SQL, including whitespace.
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
