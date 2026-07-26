package migrations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maximumMigrationFileSize = 16 << 20

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
)

var migrationFilenamePattern = regexp.MustCompile(
	`^([0-9]+)_([a-z0-9]+(?:_[a-z0-9]+)*)\.sql$`,
)

// Source loads a complete immutable migration history.
type Source interface {
	Load(context.Context) ([]Migration, error)
}

// FSSource loads canonical SQL migrations from one fs.FS directory. It works
// with embed.FS and rejects unrelated entries so packaging mistakes fail closed.
type FSSource struct {
	fs   fs.FS
	root string
}

// NewFSSource constructs a source rooted at a valid fs.FS path.
func NewFSSource(sourceFS fs.FS, root string) (*FSSource, error) {
	if sourceFS == nil || !fs.ValidPath(root) {
		return nil, ErrInvalidSource
	}

	return &FSSource{fs: sourceFS, root: root}, nil
}

// Load reads, validates, and sorts the complete migration history.
func (source *FSSource) Load(ctx context.Context) ([]Migration, error) {
	if source == nil || source.fs == nil || !fs.ValidPath(source.root) {
		return nil, ErrInvalidSource
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	entries, err := fs.ReadDir(source.fs, source.root)
	if err != nil {
		return nil, fmt.Errorf("%w: read %q: %w", ErrInvalidSource, source.root, err)
	}

	migrations := make([]Migration, 0, len(entries))
	versions := make(map[Version]string, len(entries))

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || path.Ext(entry.Name()) != ".sql" {
			return nil, fmt.Errorf("%w: %s", ErrUnexpectedSourceEntry, entry.Name())
		}

		version, name, err := parseMigrationFilename(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if prior, exists := versions[version]; exists {
			return nil, fmt.Errorf(
				"%w: version %d used by %s and %s",
				ErrDuplicateVersion,
				version,
				prior,
				entry.Name(),
			)
		}

		contents, err := readMigrationFile(source.fs, path.Join(source.root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		migration, err := parseMigrationFile(version, name, contents)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}

		versions[version] = entry.Name()
		migrations = append(migrations, migration)
	}

	sort.Slice(migrations, func(left, right int) bool {
		return migrations[left].Version() < migrations[right].Version()
	})

	return migrations, nil
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

func readMigrationFile(sourceFS fs.FS, filename string) (string, error) {
	file, err := sourceFS.Open(filename)
	if err != nil {
		return "", fmt.Errorf("%w: open: %w", ErrInvalidSource, err)
	}
	defer func() { _ = file.Close() }()

	contents, err := io.ReadAll(io.LimitReader(file, maximumMigrationFileSize+1))
	if err != nil {
		return "", fmt.Errorf("%w: read: %w", ErrInvalidSource, err)
	}
	if len(contents) > maximumMigrationFileSize ||
		!utf8.Valid(contents) ||
		strings.IndexByte(string(contents), 0) >= 0 ||
		strings.HasPrefix(string(contents), "\ufeff") {
		return "", ErrInvalidEncoding
	}

	return string(contents), nil
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
