package migrations

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultSourceTimeout          = 10 * time.Minute
	maximumMigrationFileSize      = 16 << 20
	maximumMigrationFilenameBytes = 19 + 1 + MaxMigrationNameBytes + len(".sql")
	// MaxMigrationFiles is the largest complete source inventory retained by
	// one load operation.
	MaxMigrationFiles = 4096
	// MaxMigrationSourceRootBytes is the largest source root path accepted by
	// NewFSSource.
	MaxMigrationSourceRootBytes = 4096
	// MaxMigrationSourceNameBytes bounds aggregate filename data returned by a
	// source directory operation.
	MaxMigrationSourceNameBytes = 1 << 20
	// MaxMigrationSourceBytes bounds aggregate migration-file content read by
	// one load operation.
	MaxMigrationSourceBytes = 16 << 20
)

var (
	// ErrInvalidSource indicates an unusable filesystem source configuration.
	ErrInvalidSource = errors.New("invalid migration source")
	// ErrInvalidFilename indicates a non-canonical migration filename.
	ErrInvalidFilename = errors.New("invalid migration filename")
	// ErrInvalidFormat indicates malformed or ambiguous migration directives.
	ErrInvalidFormat = errors.New("invalid migration format")
	// ErrInvalidEncoding indicates non-UTF-8, NUL-containing, or oversized input.
	ErrInvalidEncoding = errors.New("invalid migration encoding")
	// ErrUnexpectedSourceEntry indicates a non-migration entry in the source.
	ErrUnexpectedSourceEntry = errors.New("unexpected migration source entry")
	// ErrDuplicateVersion indicates that two source files claim one identity.
	ErrDuplicateVersion = errors.New("duplicate migration version")
	// ErrSourceLimit indicates a source inventory whose count, filename data, or
	// aggregate file content exceeds the finite load budget.
	ErrSourceLimit = errors.New("migration source limit exceeded")
)

var migrationFilenamePattern = regexp.MustCompile(
	`^([0-9]+)_([a-z0-9]+(?:_[a-z0-9]+)*)\.sql$`,
)

// Source loads a complete immutable migration history. Implementations must
// honor the supplied context and return at most MaxMigrationFiles entries.
type Source interface {
	Load(context.Context) ([]Migration, error)
}

// SourceEntry is the bounded directory metadata needed to discover a migration.
type SourceEntry struct {
	// Name is the base filename within the source root.
	Name string
	// Directory reports whether the entry is a directory.
	Directory bool
}

// SourceDirectoryLimits are inclusive budgets that a SourceFileSystem must
// enforce before retaining or returning directory metadata.
type SourceDirectoryLimits struct {
	// MaxEntries is the largest complete entry inventory.
	MaxEntries int
	// MaxNameBytes is the largest individual entry name.
	MaxNameBytes int
	// MaxTotalNameBytes is the largest aggregate entry-name payload.
	MaxTotalNameBytes int
}

// SourceFileSystem provides cancellation-aware, bounded migration file access.
// Implementations must pass ctx to every blocking operation, stop promptly on
// cancellation, and enforce the supplied inclusive limits before retaining
// input data. They return context errors for cancellation, ErrSourceLimit or
// ErrInvalidEncoding for exceeded budgets, and a private implementation error
// for other failures; FSSource redacts those private errors as ErrInvalidSource.
type SourceFileSystem interface {
	ReadDir(ctx context.Context, root string, limits SourceDirectoryLimits) ([]SourceEntry, error)
	ReadFile(ctx context.Context, name string, maxBytes int) ([]byte, error)
}

// FSSource loads canonical SQL migrations from one cancellation-aware source
// directory and rejects unrelated entries so packaging mistakes fail closed.
type FSSource struct {
	fs      SourceFileSystem
	root    string
	timeout time.Duration
}

// FSSourceOption configures a filesystem source.
type FSSourceOption func(*FSSource) error

// WithSourceTimeout replaces the finite default complete-load timeout.
func WithSourceTimeout(timeout time.Duration) FSSourceOption {
	return func(source *FSSource) error {
		if timeout <= 0 {
			return ErrInvalidSource
		}
		source.timeout = timeout

		return nil
	}
}

// NewFSSource constructs a source rooted at a valid fs.ValidPath directory.
func NewFSSource(sourceFS SourceFileSystem, root string, options ...FSSourceOption) (*FSSource, error) {
	if sourceFS == nil || len(root) > MaxMigrationSourceRootBytes || !fs.ValidPath(root) {
		return nil, ErrInvalidSource
	}

	source := &FSSource{fs: sourceFS, root: root, timeout: defaultSourceTimeout}
	for _, option := range options {
		if option == nil {
			return nil, ErrInvalidSource
		}
		if err := option(source); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidSource, err)
		}
	}

	return source, nil
}

// Load reads, validates, and sorts the complete migration history.
func (source *FSSource) Load(ctx context.Context) ([]Migration, error) {
	if source == nil || source.fs == nil || source.timeout <= 0 || !fs.ValidPath(source.root) {
		return nil, ErrInvalidSource
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loadCtx, cancel := context.WithTimeout(ctx, source.timeout)
	defer cancel()

	entries, err := readMigrationEntries(loadCtx, source.fs, source.root)
	if err != nil {
		return nil, err
	}

	migrations := make([]Migration, 0, len(entries))
	versions := make(map[Version]string, len(entries))
	totalBytes := 0

	for _, entry := range entries {
		if entry.Directory || path.Ext(entry.Name) != ".sql" {
			return nil, ErrUnexpectedSourceEntry
		}

		version, name, err := parseMigrationFilename(entry.Name)
		if err != nil {
			return nil, err
		}
		if prior, exists := versions[version]; exists {
			return nil, fmt.Errorf(
				"%w: version %d used by %s and %s",
				ErrDuplicateVersion,
				version,
				prior,
				entry.Name,
			)
		}

		remainingBytes := MaxMigrationSourceBytes - totalBytes
		contents, err := readMigrationFile(
			loadCtx,
			source.fs,
			path.Join(source.root, entry.Name),
			min(maximumMigrationFileSize, remainingBytes),
		)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", entry.Name, err)
		}
		totalBytes += len(contents)
		migration, err := parseMigrationFile(version, name, contents)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", entry.Name, err)
		}

		versions[version] = entry.Name
		migrations = append(migrations, migration)
	}

	slices.SortFunc(migrations, func(left, right Migration) int {
		return cmp.Compare(left.Version(), right.Version())
	})

	return migrations, nil
}

func readMigrationEntries(
	ctx context.Context,
	sourceFS SourceFileSystem,
	root string,
) ([]SourceEntry, error) {
	limits := SourceDirectoryLimits{
		MaxEntries:        MaxMigrationFiles,
		MaxNameBytes:      maximumMigrationFilenameBytes,
		MaxTotalNameBytes: MaxMigrationSourceNameBytes,
	}
	entries, err := sourceFS.ReadDir(ctx, root, limits)
	if err != nil {
		return nil, normalizeSourceError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(entries) > limits.MaxEntries {
		return nil, ErrSourceLimit
	}

	totalNameBytes := 0
	validated := make([]SourceEntry, 0, len(entries))
	for _, entry := range entries {
		if len(entry.Name) > limits.MaxNameBytes ||
			len(entry.Name) > limits.MaxTotalNameBytes-totalNameBytes {
			return nil, ErrSourceLimit
		}
		totalNameBytes += len(entry.Name)
		validated = append(validated, entry)
	}

	slices.SortFunc(validated, func(left, right SourceEntry) int {
		return strings.Compare(left.Name, right.Name)
	})

	return validated, nil
}

func parseMigrationFilename(filename string) (Version, string, error) {
	matches := migrationFilenamePattern.FindStringSubmatch(filename)
	if matches == nil {
		return 0, "", ErrInvalidFilename
	}

	parsed, err := strconv.ParseUint(matches[1], 10, 63)
	if err != nil || parsed == 0 {
		return 0, "", ErrInvalidFilename
	}

	return Version(parsed), matches[2], nil
}

func readMigrationFile(
	ctx context.Context,
	sourceFS SourceFileSystem,
	filename string,
	maxBytes int,
) (string, error) {
	contents, err := sourceFS.ReadFile(ctx, filename, maxBytes)
	if err != nil {
		if maxBytes < maximumMigrationFileSize && errors.Is(err, ErrInvalidEncoding) {
			return "", ErrSourceLimit
		}
		return "", normalizeSourceError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(contents) > maxBytes {
		if maxBytes < maximumMigrationFileSize {
			return "", ErrSourceLimit
		}
		return "", ErrInvalidEncoding
	}
	if len(contents) > maximumMigrationFileSize ||
		!utf8.Valid(contents) ||
		strings.IndexByte(string(contents), 0) >= 0 ||
		strings.HasPrefix(string(contents), "\ufeff") {
		return "", ErrInvalidEncoding
	}

	return string(contents), nil
}

func normalizeSourceError(ctx context.Context, err error) error {
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrSourceLimit) {
		return ErrSourceLimit
	}
	if errors.Is(err, ErrInvalidEncoding) {
		return ErrInvalidEncoding
	}

	return ErrInvalidSource
}

func parseMigrationFile(version Version, name string, contents string) (Migration, error) {
	const (
		directivePrefix        = "-- +migrations"
		directiveUp            = directivePrefix + " Up"
		directiveDown          = directivePrefix + " Down"
		directiveNoTransaction = directivePrefix + " NoTransaction"
	)

	mode := TransactionModeDefault
	section := ""
	seenUp := false
	seenDown := false
	seenNoTransaction := false
	var upSQL strings.Builder
	var downSQL strings.Builder

	for _, line := range strings.SplitAfter(contents, "\n") {
		directive := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")

		switch directive {
		case directiveNoTransaction:
			if seenNoTransaction || seenUp {
				return Migration{}, ErrInvalidFormat
			}
			seenNoTransaction = true
			mode = TransactionModeNone
		case directiveUp:
			if seenUp || seenDown {
				return Migration{}, ErrInvalidFormat
			}
			seenUp = true
			section = "up"
		case directiveDown:
			if !seenUp || seenDown {
				return Migration{}, ErrInvalidFormat
			}
			seenDown = true
			section = "down"
		default:
			if strings.HasPrefix(directive, directivePrefix) {
				return Migration{}, ErrInvalidFormat
			}
			switch section {
			case "up":
				upSQL.WriteString(line)
			case "down":
				downSQL.WriteString(line)
			default:
				if strings.TrimSpace(line) != "" {
					return Migration{}, ErrInvalidFormat
				}
			}
		}
	}

	if !seenUp {
		return Migration{}, ErrInvalidFormat
	}

	migration, err := NewMigration(version, name, mode, upSQL.String(), downSQL.String())
	if err != nil {
		return Migration{}, fmt.Errorf("%w: %w", ErrInvalidFormat, err)
	}

	return migration, nil
}
