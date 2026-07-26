package postgres

import (
	"testing"
	"time"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func FuzzDecodeLedgerRecord(f *testing.F) {
	f.Add(
		"migration",
		int64(1),
		"create_users",
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		int64(12),
		false,
	)
	f.Add("unknown", int64(-1), "../bad", "invalid", int64(-1), true)

	f.Fuzz(func(
		t *testing.T,
		kind string,
		version int64,
		name string,
		checksum string,
		durationMS int64,
		dirty bool,
	) {
		appliedAt := time.Unix(1_700_000_000, 0).UTC()
		record, err := decodeRecord(
			kind,
			version,
			name,
			checksum,
			appliedAt,
			durationMS,
			dirty,
		)
		if err != nil {
			return
		}
		if record.Version() != migrations.Version(version) ||
			record.Name() != name || record.AppliedAt() != appliedAt ||
			record.Duration() != time.Duration(durationMS)*time.Millisecond ||
			record.Dirty() != dirty {
			t.Fatalf("decodeRecord() changed persisted values: %#v", record)
		}
	})
}
