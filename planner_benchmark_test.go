package migrations

import (
	"fmt"
	"testing"
	"time"
)

var (
	benchmarkPlanSink   Plan
	benchmarkStatusSink Status
)

func BenchmarkPlanUp(b *testing.B) {
	benchmarkHistorySizes(b, func(b *testing.B, migrations []Migration, records []Record) {
		var plan Plan
		var err error
		for b.Loop() {
			plan, err = PlanUp(migrations, records)
			if err != nil {
				b.Fatal(err)
			}
		}
		benchmarkPlanSink = plan
	})
}

func BenchmarkBuildStatus(b *testing.B) {
	benchmarkHistorySizes(b, func(b *testing.B, migrations []Migration, records []Record) {
		var status Status
		var err error
		for b.Loop() {
			status, err = BuildStatus(migrations, records)
			if err != nil {
				b.Fatal(err)
			}
		}
		benchmarkStatusSink = status
	})
}

func benchmarkHistorySizes(
	b *testing.B,
	benchmark func(*testing.B, []Migration, []Record),
) {
	b.Helper()
	for _, count := range []int{100, 10_000} {
		migrations, records := benchmarkHistory(b, count, count*3/4)
		b.Run(fmt.Sprintf("migrations_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(count), "migrations/op")
			benchmark(b, migrations, records)
		})
	}
}

func benchmarkHistory(b *testing.B, count int, applied int) ([]Migration, []Record) {
	b.Helper()
	migrations := make([]Migration, 0, count)
	records := make([]Record, 0, applied)
	appliedAt := time.Unix(1_700_000_000, 0).UTC()

	for index := 1; index <= count; index++ {
		name := fmt.Sprintf("migration_%d", index)
		migration, err := NewMigration(
			Version(index),
			name,
			TransactionModeDefault,
			fmt.Sprintf("CREATE TABLE table_%d (id bigint);", index),
			fmt.Sprintf("DROP TABLE table_%d;", index),
		)
		if err != nil {
			b.Fatal(err)
		}
		migrations = append(migrations, migration)

		if index <= applied {
			record, recordErr := NewRecord(
				RecordKindMigration,
				migration.Version(),
				migration.Name(),
				migration.Checksum(),
				appliedAt,
				time.Millisecond,
				false,
			)
			if recordErr != nil {
				b.Fatal(recordErr)
			}
			records = append(records, record)
		}
	}

	return migrations, records
}
