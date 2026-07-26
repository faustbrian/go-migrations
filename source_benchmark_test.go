package migrations

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

var (
	benchmarkMigrationSink  Migration
	benchmarkMigrationsSink []Migration
)

func BenchmarkParseMigrationFile(b *testing.B) {
	for _, size := range []int{1 << 10, 1 << 20} {
		contents := benchmarkMigrationContents(size)
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(contents)))

			var migration Migration
			for b.Loop() {
				parsed, err := parseMigrationFile(1, "create_widgets", contents)
				if err != nil {
					b.Fatal(err)
				}
				migration = parsed
			}
			benchmarkMigrationSink = migration
		})
	}
}

func BenchmarkFSSourceLoad(b *testing.B) {
	for _, count := range []int{100, 1000} {
		files, totalBytes := benchmarkMigrationFS(count, 2<<10)
		source, err := NewFSSource(files, "migrations")
		if err != nil {
			b.Fatal(err)
		}

		b.Run(fmt.Sprintf("migrations_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(count), "migrations/op")
			b.SetBytes(int64(totalBytes))

			var loaded []Migration
			for b.Loop() {
				loaded, err = source.Load(context.Background())
				if err != nil {
					b.Fatal(err)
				}
			}
			benchmarkMigrationsSink = loaded
		})
	}
}

func benchmarkMigrationFS(count int, bytesPerFile int) (fstest.MapFS, int) {
	files := make(fstest.MapFS, count)
	totalBytes := 0
	for index := count; index > 0; index-- {
		contents := benchmarkMigrationContents(bytesPerFile)
		filename := fmt.Sprintf("migrations/%d_migration_%d.sql", index, index)
		files[filename] = &fstest.MapFile{Data: []byte(contents)}
		totalBytes += len(contents)
	}

	return files, totalBytes
}

func benchmarkMigrationContents(size int) string {
	const prefix = "-- +migrations Up\nCREATE TABLE widgets (id bigint);\n-- payload\n"
	const suffix = "\n-- +migrations Down\nDROP TABLE widgets;\n"

	payloadSize := max(0, size-len(prefix)-len(suffix))

	return prefix + strings.Repeat("x", payloadSize) + suffix
}
