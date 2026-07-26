package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func FuzzFSSource(f *testing.F) {
	f.Add("000001_create_users.sql", "-- +migrations Up\nSELECT 1;\n")
	f.Add("1_index_users.sql", "-- +migrations NoTransaction\n-- +migrations Up\nSELECT $$a;b$$;\n")
	f.Add("invalid.sql", "-- +migrations Sideways\n\x00")

	f.Fuzz(func(t *testing.T, filename string, contents string) {
		if filename == "" || filename == "." || filename == ".." ||
			!fs.ValidPath("migrations/"+filename) {
			return
		}
		source, err := migrations.NewFSSource(fstest.MapFS{
			"migrations/" + filename: &fstest.MapFile{Data: []byte(contents)},
		}, "migrations")
		if err != nil {
			t.Fatalf("NewFSSource() error = %v", err)
		}
		loaded, err := source.Load(context.Background())
		if err != nil {
			return
		}
		if len(loaded) != 1 {
			t.Fatalf("Load() count = %d, want 1", len(loaded))
		}
		migration := loaded[0]
		if migration.Version() == 0 || migration.Name() == "" ||
			migration.UpSQL() == "" || migration.Checksum() == (migrations.Checksum{}) {
			t.Fatalf("Load() returned invalid migration: %#v", migration)
		}
	})
}

func FuzzMigrationIdentity(f *testing.F) {
	f.Add(uint64(1), "create_users", uint8(0), "SELECT 1;", "SELECT 2;")
	f.Add(uint64(0), "../users", uint8(255), "", "")

	f.Fuzz(func(
		t *testing.T,
		version uint64,
		name string,
		mode uint8,
		upSQL string,
		downSQL string,
	) {
		migration, err := migrations.NewMigration(
			migrations.Version(version),
			name,
			migrations.TransactionMode(mode),
			upSQL,
			downSQL,
		)
		if err != nil {
			return
		}
		if migration.Version() != migrations.Version(version) ||
			migration.Name() != name || migration.UpSQL() != upSQL ||
			migration.DownSQL() != downSQL {
			t.Fatalf("NewMigration() changed canonical input: %s", fmt.Sprint(migration))
		}
		parsed, err := migrations.ParseChecksum(migration.Checksum().String())
		if err != nil || parsed != migration.Checksum() {
			t.Fatalf("checksum round trip = %v, %v", parsed, err)
		}
	})
}

func FuzzChecksum(f *testing.F) {
	f.Add("sha256:74ec4f716c2502dabb1388ec2d41313ed04fed35729dd9221feb5d5972535801")
	f.Add("sha256:")
	f.Add("md5:abcd")

	f.Fuzz(func(t *testing.T, encoded string) {
		checksum, err := migrations.ParseChecksum(encoded)
		if err != nil {
			return
		}
		if checksum.String() != encoded {
			t.Fatalf("checksum canonicalization changed %q to %q", encoded, checksum)
		}
	})
}
