package migrations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
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
			source, err := NewFSSource(mapSourceFileSystem{files: fstest.MapFS{
				"migrations/1_test.sql": &fstest.MapFile{Data: []byte(test.content)},
			}}, "migrations")
			if err != nil {
				t.Fatalf("NewFSSource() error = %v", err)
			}
			_, err = source.Load(context.Background())
			if !errors.Is(err, test.target) {
				t.Fatalf("Load() error = %v, want %v", err, test.target)
			}
		})
	}

	source, err := NewFSSource(mapSourceFileSystem{files: fstest.MapFS{
		"migrations/1_large.sql": &fstest.MapFile{Data: []byte(strings.Repeat("x", maximumMigrationFileSize+1))},
	}}, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := source.Load(context.Background()); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("Load(oversized) error = %v, want ErrInvalidEncoding", err)
	}
}

func TestSourceRejectsMigrationCountBeyondFiniteBudget(t *testing.T) {
	t.Parallel()

	files := make(fstest.MapFS, MaxMigrationFiles+1)
	for index := 1; index <= MaxMigrationFiles+1; index++ {
		filename := fmt.Sprintf("migrations/%06d_entry.sql", index)
		files[filename] = &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT 1;\n")}
	}
	source, err := NewFSSource(mapSourceFileSystem{files: files}, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := source.Load(context.Background()); !errors.Is(err, ErrSourceLimit) {
		t.Fatalf("Load() error = %v, want ErrSourceLimit", err)
	}
}

func TestSourceRejectsAggregateSQLBeyondFiniteBudget(t *testing.T) {
	t.Parallel()

	const header = "-- +migrations Up\n"
	contents := []byte(header + strings.Repeat("x", MaxMigrationSourceBytes/2-len(header)))
	source, err := NewFSSource(mapSourceFileSystem{files: fstest.MapFS{
		"migrations/000001_first.sql":  &fstest.MapFile{Data: contents},
		"migrations/000002_second.sql": &fstest.MapFile{Data: contents},
		"migrations/000003_third.sql":  &fstest.MapFile{Data: contents},
	}}, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := source.Load(context.Background()); !errors.Is(err, ErrSourceLimit) {
		t.Fatalf("Load() error = %v, want ErrSourceLimit", err)
	}
}

func TestSourceBoundsNamesBeforeInventoryProcessing(t *testing.T) {
	t.Parallel()

	exactName := "9223372036854775807_" + strings.Repeat("a", MaxMigrationNameBytes) + ".sql"
	exact, err := readMigrationEntries(context.Background(), fixedSourceFileSystem{
		entries: []SourceEntry{{Name: exactName}},
	}, ".")
	if err != nil {
		t.Fatalf("readMigrationEntries(exact name) error = %v", err)
	}
	if len(exact) != 1 || exact[0].Name != exactName {
		t.Fatalf("readMigrationEntries(exact name) = %#v", exact)
	}

	_, err = readMigrationEntries(context.Background(), fixedSourceFileSystem{
		entries: []SourceEntry{{Name: exactName + "a"}},
	}, ".")
	if !errors.Is(err, ErrSourceLimit) {
		t.Fatalf("readMigrationEntries(oversized name) error = %v, want ErrSourceLimit", err)
	}
}

func TestSourceBoundsAggregateInventoryNameBytes(t *testing.T) {
	t.Parallel()

	entries := make([]SourceEntry, MaxMigrationFiles)
	for index := range entries {
		entries[index] = SourceEntry{
			Name: fmt.Sprintf("%019d_%s.sql", index+1, strings.Repeat("a", 232)),
		}
	}
	if _, err := readMigrationEntries(context.Background(), fixedSourceFileSystem{entries: entries}, "."); err != nil {
		t.Fatalf("readMigrationEntries(exact aggregate names) error = %v", err)
	}

	entries[len(entries)-1].Name += "a"
	if _, err := readMigrationEntries(context.Background(), fixedSourceFileSystem{entries: entries}, "."); !errors.Is(err, ErrSourceLimit) {
		t.Fatalf("readMigrationEntries(aggregate names) error = %v, want ErrSourceLimit", err)
	}
}

func TestFSSourcePassesCancellationToEveryBlockingOperation(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"read directory", "read file"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			filesystem := &blockingSourceFileSystem{
				operation: operation,
				entered:   make(chan struct{}),
			}
			source, err := NewFSSource(filesystem, ".")
			if err != nil {
				t.Fatalf("NewFSSource() error = %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				_, loadErr := source.Load(ctx)
				result <- loadErr
			}()

			<-filesystem.entered
			cancel()
			select {
			case loadErr := <-result:
				if !errors.Is(loadErr, context.Canceled) {
					t.Fatalf("Load() error = %v, want context.Canceled", loadErr)
				}
			case <-time.After(time.Second):
				t.Fatal("Load() did not release the blocking filesystem operation")
			}
		})
	}
}

func TestFSSourceDerivesFiniteDeadlineForEveryBlockingOperation(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"read directory", "read file"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			filesystem := &blockingSourceFileSystem{
				operation: operation,
				entered:   make(chan struct{}),
			}
			source, err := NewFSSource(filesystem, ".", WithSourceTimeout(time.Millisecond))
			if err != nil {
				t.Fatalf("NewFSSource() error = %v", err)
			}
			_, err = source.Load(context.Background())
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Load() error = %v, want context.DeadlineExceeded", err)
			}
		})
	}
}

func TestFSSourceRejectsNonPositiveTimeout(t *testing.T) {
	t.Parallel()

	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := NewFSSource(fixedSourceFileSystem{}, ".", WithSourceTimeout(timeout)); !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("NewFSSource(timeout %s) error = %v, want ErrInvalidSource", timeout, err)
		}
	}
	if _, err := NewFSSource(fixedSourceFileSystem{}, ".", nil); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("NewFSSource(nil option) error = %v, want ErrInvalidSource", err)
	}
}

func TestFSSourcePassesFiniteBudgetsToProviderAndRevalidatesResults(t *testing.T) {
	t.Parallel()

	filesystem := &budgetAssertingSourceFileSystem{t: t}
	source, err := NewFSSource(filesystem, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := source.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	oversized, err := NewFSSource(oversizedFileSourceFileSystem{}, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource(oversized) error = %v", err)
	}
	if _, err := oversized.Load(context.Background()); !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("Load(oversized provider result) error = %v, want ErrInvalidEncoding", err)
	}
}

func TestFSSourcePassesRemainingAggregateContentBudget(t *testing.T) {
	t.Parallel()

	filesystem := &remainingBudgetSourceFileSystem{}
	source, err := NewFSSource(filesystem, "migrations")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := source.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []int{MaxMigrationSourceBytes, MaxMigrationSourceBytes - len(validMigrationContents)}
	if !slices.Equal(filesystem.budgets, want) {
		t.Fatalf("ReadFile() budgets = %v, want %v", filesystem.budgets, want)
	}
}

func TestSourceChecksCancellationAfterProvider(t *testing.T) {
	t.Parallel()

	directoryCtx, cancelDirectory := context.WithCancel(context.Background())
	if _, err := readMigrationEntries(directoryCtx, cancelingSourceFileSystem{
		cancel:    cancelDirectory,
		operation: "read directory",
	}, "."); !errors.Is(err, context.Canceled) {
		t.Fatalf("readMigrationEntries(canceled return) error = %v, want context.Canceled", err)
	}

	fileCtx, cancelFile := context.WithCancel(context.Background())
	if _, err := readMigrationFile(fileCtx, cancelingSourceFileSystem{
		cancel:    cancelFile,
		operation: "read file",
	}, "1_test.sql", maximumMigrationFileSize); !errors.Is(err, context.Canceled) {
		t.Fatalf("readMigrationFile(canceled return) error = %v, want context.Canceled", err)
	}

}

func TestSourceNormalizesDetachedProviderCancellation(t *testing.T) {
	t.Parallel()

	for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
		if actual := normalizeSourceError(context.Background(), fmt.Errorf("provider: %w", target)); !errors.Is(actual, target) {
			t.Fatalf("normalizeSourceError(%v) = %v", target, actual)
		}
	}
}

func TestSourceHonorsConfigurationIOAndCancellationFailures(t *testing.T) {
	t.Parallel()

	var source *FSSource
	if _, err := source.Load(context.Background()); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil Load() error = %v", err)
	}

	missing, err := NewFSSource(mapSourceFileSystem{files: fstest.MapFS{}}, "missing")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := missing.Load(context.Background()); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("Load(missing) error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	valid, err := NewFSSource(mapSourceFileSystem{files: fstest.MapFS{}}, ".")
	if err != nil {
		t.Fatalf("NewFSSource() error = %v", err)
	}
	if _, err := valid.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(canceled) error = %v", err)
	}

	if _, err := readMigrationFile(context.Background(), errorSourceFileSystem{}, "migration.sql", maximumMigrationFileSize); !errors.Is(err, ErrInvalidSource) || strings.Contains(err.Error(), "private") {
		t.Fatalf("readMigrationFile(provider error) = %v, want redacted ErrInvalidSource", err)
	}
	if _, err := readMigrationEntries(context.Background(), errorSourceFileSystem{}, "."); !errors.Is(err, ErrInvalidSource) || strings.Contains(err.Error(), "private") {
		t.Fatalf("readMigrationEntries(provider error) = %v, want redacted ErrInvalidSource", err)
	}
	if _, err := readMigrationEntries(context.Background(), oversizedSourceFileSystem{}, "."); !errors.Is(err, ErrSourceLimit) {
		t.Fatalf("readMigrationEntries(oversized fallback) error = %v", err)
	}
	if _, err := readMigrationFile(context.Background(), oversizedFileSourceFileSystem{}, "migration.sql", 1); !errors.Is(err, ErrSourceLimit) {
		t.Fatalf("readMigrationFile(aggregate fallback) error = %v, want ErrSourceLimit", err)
	}

	if _, _, err := parseMigrationFilename("18446744073709551616_too_large.sql"); !errors.Is(err, ErrInvalidFilename) {
		t.Fatalf("parseMigrationFilename(overflow) error = %v", err)
	}
}

type mapSourceFileSystem struct {
	files fs.FS
}

func (filesystem mapSourceFileSystem) ReadDir(
	ctx context.Context,
	root string,
	limits SourceDirectoryLimits,
) ([]SourceEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(filesystem.files, root)
	if err != nil {
		return nil, err
	}
	if len(entries) > limits.MaxEntries {
		return nil, ErrSourceLimit
	}
	converted := make([]SourceEntry, 0, len(entries))
	totalNameBytes := 0
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > limits.MaxNameBytes || len(name) > limits.MaxTotalNameBytes-totalNameBytes {
			return nil, ErrSourceLimit
		}
		totalNameBytes += len(name)
		converted = append(converted, SourceEntry{Name: name, Directory: entry.IsDir()})
	}

	return converted, ctx.Err()
}

func (filesystem mapSourceFileSystem) ReadFile(ctx context.Context, name string, maxBytes int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contents, err := fs.ReadFile(filesystem.files, name)
	if err != nil {
		return nil, err
	}
	if len(contents) > maxBytes {
		return nil, ErrInvalidEncoding
	}

	return contents, ctx.Err()
}

type errorSourceFileSystem struct{}

func (errorSourceFileSystem) ReadDir(context.Context, string, SourceDirectoryLimits) ([]SourceEntry, error) {
	return nil, errors.New("read directory failed: private")
}

func (errorSourceFileSystem) ReadFile(context.Context, string, int) ([]byte, error) {
	return nil, errors.New("read file failed: private")
}

type oversizedSourceFileSystem struct{}

func (oversizedSourceFileSystem) ReadDir(
	context.Context,
	string,
	SourceDirectoryLimits,
) ([]SourceEntry, error) {
	return make([]SourceEntry, MaxMigrationFiles+1), nil
}

func (oversizedSourceFileSystem) ReadFile(context.Context, string, int) ([]byte, error) {
	return nil, errors.New("unexpected file read")
}

type budgetAssertingSourceFileSystem struct {
	t *testing.T
}

const validMigrationContents = "-- +migrations Up\nSELECT 1;\n"

type remainingBudgetSourceFileSystem struct {
	budgets []int
}

func (*remainingBudgetSourceFileSystem) ReadDir(
	context.Context,
	string,
	SourceDirectoryLimits,
) ([]SourceEntry, error) {
	return []SourceEntry{{Name: "1_first.sql"}, {Name: "2_second.sql"}}, nil
}

func (filesystem *remainingBudgetSourceFileSystem) ReadFile(
	_ context.Context,
	_ string,
	maxBytes int,
) ([]byte, error) {
	filesystem.budgets = append(filesystem.budgets, maxBytes)

	return []byte(validMigrationContents), nil
}

func (filesystem *budgetAssertingSourceFileSystem) ReadDir(
	ctx context.Context,
	root string,
	limits SourceDirectoryLimits,
) ([]SourceEntry, error) {
	filesystem.t.Helper()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	want := SourceDirectoryLimits{
		MaxEntries:        MaxMigrationFiles,
		MaxNameBytes:      maximumMigrationFilenameBytes,
		MaxTotalNameBytes: MaxMigrationSourceNameBytes,
	}
	if root != "migrations" || limits != want {
		filesystem.t.Fatalf("ReadDir(%q) limits = %#v, want %#v", root, limits, want)
	}

	return []SourceEntry{{Name: "1_test.sql"}}, nil
}

func (filesystem *budgetAssertingSourceFileSystem) ReadFile(
	ctx context.Context,
	name string,
	maxBytes int,
) ([]byte, error) {
	filesystem.t.Helper()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name != "migrations/1_test.sql" || maxBytes != maximumMigrationFileSize {
		filesystem.t.Fatalf("ReadFile(%q) maxBytes = %d", name, maxBytes)
	}

	return []byte("-- +migrations Up\nSELECT 1;\n"), nil
}

type oversizedFileSourceFileSystem struct{}

func (oversizedFileSourceFileSystem) ReadDir(
	context.Context,
	string,
	SourceDirectoryLimits,
) ([]SourceEntry, error) {
	return []SourceEntry{{Name: "1_test.sql"}}, nil
}

func (oversizedFileSourceFileSystem) ReadFile(context.Context, string, int) ([]byte, error) {
	return make([]byte, maximumMigrationFileSize+1), nil
}

type fixedSourceFileSystem struct {
	entries []SourceEntry
}

func (filesystem fixedSourceFileSystem) ReadDir(
	context.Context,
	string,
	SourceDirectoryLimits,
) ([]SourceEntry, error) {
	return filesystem.entries, nil
}

func (fixedSourceFileSystem) ReadFile(context.Context, string, int) ([]byte, error) {
	return []byte("-- +migrations Up\nSELECT 1;\n"), nil
}

type blockingSourceFileSystem struct {
	operation string
	entered   chan struct{}
}

func (filesystem *blockingSourceFileSystem) ReadDir(
	ctx context.Context,
	_ string,
	_ SourceDirectoryLimits,
) ([]SourceEntry, error) {
	if filesystem.operation == "read directory" {
		close(filesystem.entered)
		<-ctx.Done()

		return nil, ctx.Err()
	}

	return []SourceEntry{{Name: "1_test.sql"}}, nil
}

func (filesystem *blockingSourceFileSystem) ReadFile(
	ctx context.Context,
	_ string,
	_ int,
) ([]byte, error) {
	close(filesystem.entered)
	<-ctx.Done()

	return nil, ctx.Err()
}

type cancelingSourceFileSystem struct {
	cancel    context.CancelFunc
	operation string
}

func (filesystem cancelingSourceFileSystem) ReadDir(
	context.Context,
	string,
	SourceDirectoryLimits,
) ([]SourceEntry, error) {
	if filesystem.operation == "read directory" {
		filesystem.cancel()
	}

	return []SourceEntry{{Name: "1_test.sql"}}, nil
}

func (filesystem cancelingSourceFileSystem) ReadFile(context.Context, string, int) ([]byte, error) {
	if filesystem.operation == "read file" {
		filesystem.cancel()
	}

	return []byte("-- +migrations Up\nSELECT 1;\n"), nil
}
