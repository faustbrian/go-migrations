package migrations_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	migrations "github.com/faustbrian/go-migrations/v3"
)

func TestFSSourceLoadsCanonicalMigrationsInVersionOrder(t *testing.T) {
	t.Parallel()

	source, err := migrations.NewFSSource(testSourceFileSystem{files: fstest.MapFS{
		"migrations/000002_add_email.sql": &fstest.MapFile{Data: []byte(
			"-- +migrations NoTransaction\n" +
				"-- +migrations Up\n" +
				"CREATE INDEX CONCURRENTLY users_email_idx ON users (email);\n" +
				"-- +migrations Down\n" +
				"DROP INDEX CONCURRENTLY users_email_idx;\n",
		)},
		"migrations/000001_create_users.sql": &fstest.MapFile{Data: []byte(
			"-- +migrations Up\n" +
				"CREATE TABLE users (id bigint PRIMARY KEY);\n" +
				"-- a semicolon in a PostgreSQL dollar quote is SQL, not syntax\n" +
				"SELECT $$a;b$$;\n" +
				"-- +migrations Down\n" +
				"DROP TABLE users;\n",
		)},
	}}, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}

	loaded, err := source.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("Load() count = %d, want 2", len(loaded))
	}
	if loaded[0].Version() != 1 || loaded[0].Name() != "create_users" {
		t.Fatalf("Load()[0] = %d_%s", loaded[0].Version(), loaded[0].Name())
	}
	if loaded[0].TransactionMode() != migrations.TransactionModeDefault {
		t.Fatalf("Load()[0].TransactionMode() = %v", loaded[0].TransactionMode())
	}
	if loaded[1].Version() != 2 || loaded[1].TransactionMode() != migrations.TransactionModeNone {
		t.Fatalf("Load()[1] = %#v", loaded[1])
	}
	if loaded[0].UpSQL() != "CREATE TABLE users (id bigint PRIMARY KEY);\n-- a semicolon in a PostgreSQL dollar quote is SQL, not syntax\nSELECT $$a;b$$;\n" {
		t.Fatalf("Load()[0].UpSQL() = %q", loaded[0].UpSQL())
	}
}

func TestFSSourceRejectsAmbiguousOrHostileFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		files       fstest.MapFS
		targetError error
	}{
		{
			name: "invalid filename",
			files: fstest.MapFS{
				"migrations/create_users.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT 1;\n")},
			},
			targetError: migrations.ErrInvalidFilename,
		},
		{
			name: "version exceeds ledger",
			files: fstest.MapFS{
				"migrations/9223372036854775808_create_users.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT 1;\n")},
			},
			targetError: migrations.ErrInvalidFilename,
		},
		{
			name: "missing up directive",
			files: fstest.MapFS{
				"migrations/000001_create_users.sql": &fstest.MapFile{Data: []byte("SELECT 1;\n")},
			},
			targetError: migrations.ErrInvalidFormat,
		},
		{
			name: "duplicate up directive",
			files: fstest.MapFS{
				"migrations/000001_create_users.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT 1;\n-- +migrations Up\nSELECT 2;\n")},
			},
			targetError: migrations.ErrInvalidFormat,
		},
		{
			name: "duplicate version",
			files: fstest.MapFS{
				"migrations/000001_create_users.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT 1;\n")},
				"migrations/1_create_accounts.sql":   &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT 2;\n")},
			},
			targetError: migrations.ErrDuplicateVersion,
		},
		{
			name: "invalid UTF-8",
			files: fstest.MapFS{
				"migrations/000001_create_users.sql": &fstest.MapFile{Data: []byte{'-', '-', ' ', '+', 'm', 'i', 'g', 'r', 'a', 't', 'i', 'o', 'n', 's', ' ', 'U', 'p', '\n', 0xff}},
			},
			targetError: migrations.ErrInvalidEncoding,
		},
		{
			name: "unexpected file extension",
			files: fstest.MapFS{
				"migrations/README.md": &fstest.MapFile{Data: []byte("documentation")},
			},
			targetError: migrations.ErrUnexpectedSourceEntry,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			source, err := migrations.NewFSSource(testSourceFileSystem{files: test.files}, "migrations")
			if err != nil {
				t.Fatalf("NewFSSource() error = %v", err)
			}
			_, err = source.Load(context.Background())
			if !errors.Is(err, test.targetError) {
				t.Fatalf("Load() error = %v, want %v", err, test.targetError)
			}
		})
	}
}

func TestNewFSSourceRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := migrations.NewFSSource(nil, "."); !errors.Is(err, migrations.ErrInvalidSource) {
		t.Fatalf("NewFSSource(nil) error = %v, want ErrInvalidSource", err)
	}
	if _, err := migrations.NewFSSource(testSourceFileSystem{files: fstest.MapFS{}}, "../migrations"); !errors.Is(err, migrations.ErrInvalidSource) {
		t.Fatalf("NewFSSource(path) error = %v, want ErrInvalidSource", err)
	}
}

func TestNewFSSourceBoundsRootBytesBeforePathValidation(t *testing.T) {
	t.Parallel()

	maximumRoot := strings.Repeat("a", migrations.MaxMigrationSourceRootBytes)
	if _, err := migrations.NewFSSource(testSourceFileSystem{files: fstest.MapFS{}}, maximumRoot); err != nil {
		t.Fatalf("NewFSSource(maximum root) error = %v", err)
	}
	if _, err := migrations.NewFSSource(testSourceFileSystem{files: fstest.MapFS{}}, maximumRoot+"a"); !errors.Is(err, migrations.ErrInvalidSource) {
		t.Fatalf("NewFSSource(oversized root) error = %v, want ErrInvalidSource", err)
	}
}

type testSourceFileSystem struct {
	files fs.FS
}

func (filesystem testSourceFileSystem) ReadDir(
	ctx context.Context,
	root string,
	limits migrations.SourceDirectoryLimits,
) ([]migrations.SourceEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(filesystem.files, root)
	if err != nil {
		return nil, err
	}
	if len(entries) > limits.MaxEntries {
		return nil, migrations.ErrSourceLimit
	}
	converted := make([]migrations.SourceEntry, 0, len(entries))
	totalNameBytes := 0
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > limits.MaxNameBytes || len(name) > limits.MaxTotalNameBytes-totalNameBytes {
			return nil, migrations.ErrSourceLimit
		}
		totalNameBytes += len(name)
		converted = append(converted, migrations.SourceEntry{Name: name, Directory: entry.IsDir()})
	}

	return converted, ctx.Err()
}

func (filesystem testSourceFileSystem) ReadFile(
	ctx context.Context,
	name string,
	maxBytes int,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contents, err := fs.ReadFile(filesystem.files, name)
	if err != nil {
		return nil, err
	}
	if len(contents) > maxBytes {
		return nil, migrations.ErrInvalidEncoding
	}

	return contents, ctx.Err()
}
