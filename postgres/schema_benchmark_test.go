package postgres

import (
	"fmt"
	"strings"
	"testing"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

var benchmarkFingerprintSink migrations.Checksum

func BenchmarkFingerprint(b *testing.B) {
	for _, count := range []int{100, 10_000} {
		objects := benchmarkSchemaObjects(count)
		b.Run(fmt.Sprintf("objects_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(count), "objects/op")

			var checksum migrations.Checksum
			for b.Loop() {
				fingerprint, err := Fingerprint(objects)
				if err != nil {
					b.Fatal(err)
				}
				checksum = fingerprint
			}
			benchmarkFingerprintSink = checksum
		})
	}
}

func benchmarkSchemaObjects(count int) []SchemaObject {
	objects := make([]SchemaObject, 0, count)
	for index := count; index > 0; index-- {
		objects = append(objects, SchemaObject{
			Identity:   fmt.Sprintf("column:public.table_%06d.value", index),
			Definition: "text not-null default='' " + strings.Repeat("x", 128),
		})
	}

	return objects
}
