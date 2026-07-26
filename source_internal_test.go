package migrations

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestSourceRejectsEveryDirectiveAndEncodingAmbiguity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		target  error
	}{
		{name: "byte order mark", content: "\ufeff-- +migrations Up\nSELECT 1;", target: ErrInvalidEncoding},
		{name: "nul", content: "-- +migrations Up\nSELECT \x00;", target: ErrInvalidEncoding},
		{name: "unknown directive", content: "-- +migrations Upward\n", target: ErrInvalidFormat},
		{name: "no transaction after up", content: "-- +migrations Up\n-- +migrations NoTransaction\nSELECT 1;", target: ErrInvalidFormat},
		{name: "duplicate no transaction", content: "-- +migrations NoTransaction\n-- +migrations NoTransaction\n-- +migrations Up\nSELECT 1;", target: ErrInvalidFormat},
		{name: "down before up", content: "-- +migrations Down\nSELECT 1;", target: ErrInvalidFormat},
		{name: "duplicate down", content: "-- +migrations Up\nSELECT 1;\n-- +migrations Down\nSELECT 2;\n-- +migrations Down\n", target: ErrInvalidFormat},
		{name: "content before up", content: "SELECT 1;\n-- +migrations Up\nSELECT 2;", target: ErrInvalidFormat},
		{name: "empty up", content: "-- +migrations Up\n\n", target: ErrInvalidFormat},
		{name: "empty down", content: "-- +migrations Up\nSELECT 1;\n-- +migrations Down\n \n", target: ErrInvalidFormat},
		{name: "empty file", content: "", target: ErrInvalidFormat},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, err := NewFSSource(fstest.MapFS{
				"migrations/1_test.sql": &fstest.MapFile{Data: []byte(test.content)},
			}, "migrations")
			if err != nil {
				t.Fatalf("NewFSSource() error = %v", err)
			}
			_, err = source.Load(context.Background())
			if !errors.Is(err, test.target) {
				t.Fatalf("Load() error = %v, want %v", err, test.target)
			}
		})
	}

	source, err := NewFSSource(fstest.MapFS{
		"migrations/1_large.sql": &fstest.MapFile{Data: []byte(strings.Repeat("x", maximumMigrationFileSize+1))},
	}, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := source.Load(context.Background()); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("Load(oversized) error = %v, want ErrInvalidEncoding", err)
	}
}

func TestSourceHonorsConfigurationIOAndCancellationFailures(t *testing.T) {
	t.Parallel()

	var source *FSSource
	if _, err := source.Load(context.Background()); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil Load() error = %v", err)
	}

	missing, err := NewFSSource(fstest.MapFS{}, "missing")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := missing.Load(context.Background()); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("Load(missing) error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	valid, err := NewFSSource(fstest.MapFS{}, ".")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := valid.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(canceled) error = %v", err)
	}

	loopCtx, loopCancel := context.WithCancel(context.Background())
	canceling, err := NewFSSource(cancelReadDirFS{cancel: loopCancel}, ".")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := canceling.Load(loopCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(canceled loop) error = %v", err)
	}

	if _, err := readMigrationFile(errorOpenFS{}, "migration.sql"); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("readMigrationFile(open) error = %v", err)
	}
	if _, err := readMigrationFile(readErrorFS{}, "migration.sql"); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("readMigrationFile(read) error = %v", err)
	}

	if _, _, err := parseMigrationFilename("18446744073709551616_too_large.sql"); !errors.Is(err, ErrInvalidFilename) {
		t.Fatalf("parseMigrationFilename(overflow) error = %v", err)
	}
}

type errorOpenFS struct{}

func (errorOpenFS) Open(string) (fs.File, error) {
	return nil, errors.New("open failed")
}

type readErrorFS struct{}

func (readErrorFS) Open(string) (fs.File, error) { return readErrorFile{}, nil }

type readErrorFile struct{}

func (readErrorFile) Stat() (fs.FileInfo, error) { return readErrorInfo{}, nil }
func (readErrorFile) Read([]byte) (int, error)   { return 0, errors.New("read failed") }
func (readErrorFile) Close() error               { return nil }

type readErrorInfo struct{}

func (readErrorInfo) Name() string       { return "migration.sql" }
func (readErrorInfo) Size() int64        { return 0 }
func (readErrorInfo) Mode() fs.FileMode  { return 0 }
func (readErrorInfo) ModTime() time.Time { return time.Time{} }
func (readErrorInfo) IsDir() bool        { return false }
func (readErrorInfo) Sys() any           { return nil }

type cancelReadDirFS struct {
	cancel context.CancelFunc
}

func (filesystem cancelReadDirFS) Open(string) (fs.File, error) {
	return nil, errors.New("unexpected Open call")
}

func (filesystem cancelReadDirFS) ReadDir(string) ([]fs.DirEntry, error) {
	filesystem.cancel()

	return []fs.DirEntry{cancelDirEntry{}}, nil
}

type cancelDirEntry struct{}

func (cancelDirEntry) Name() string               { return "1_canceled.sql" }
func (cancelDirEntry) IsDir() bool                { return false }
func (cancelDirEntry) Type() fs.FileMode          { return 0 }
func (cancelDirEntry) Info() (fs.FileInfo, error) { return readErrorInfo{}, nil }
