package migrations_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func TestNewMigrationCreatesImmutableCanonicalIdentity(t *testing.T) {
	t.Parallel()

	migration, err := migrations.NewMigration(
		42,
		"create_users",
		migrations.TransactionModeDefault,
		"CREATE TABLE users (id bigint PRIMARY KEY);\n",
		"DROP TABLE users;\n",
	)
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}

	if migration.Version() != 42 {
		t.Fatalf("Version() = %d, want 42", migration.Version())
	}
	if migration.Name() != "create_users" {
		t.Fatalf("Name() = %q, want create_users", migration.Name())
	}
	if migration.TransactionMode() != migrations.TransactionModeDefault {
		t.Fatalf("TransactionMode() = %v, want default", migration.TransactionMode())
	}
	if migration.UpSQL() != "CREATE TABLE users (id bigint PRIMARY KEY);\n" {
		t.Fatalf("UpSQL() = %q", migration.UpSQL())
	}
	if migration.DownSQL() != "DROP TABLE users;\n" {
		t.Fatalf("DownSQL() = %q", migration.DownSQL())
	}
	if migration.Checksum().String() != "sha256:74ec4f716c2502dabb1388ec2d41313ed04fed35729dd9221feb5d5972535801" {
		t.Fatalf("Checksum() = %q", migration.Checksum())
	}
}

func TestNewMigrationRejectsInvalidCanonicalIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		version     migrations.Version
		migration   string
		mode        migrations.TransactionMode
		upSQL       string
		downSQL     string
		targetError error
	}{
		{name: "zero version", migration: "create_users", upSQL: "SELECT 1;", targetError: migrations.ErrInvalidVersion},
		{name: "version exceeds ledger", version: migrations.Version(math.MaxInt64) + 1, migration: "create_users", upSQL: "SELECT 1;", targetError: migrations.ErrInvalidVersion},
		{name: "empty name", version: 1, upSQL: "SELECT 1;", targetError: migrations.ErrInvalidName},
		{name: "path name", version: 1, migration: "dir/create_users", upSQL: "SELECT 1;", targetError: migrations.ErrInvalidName},
		{name: "invalid mode", version: 1, migration: "create_users", mode: 99, upSQL: "SELECT 1;", targetError: migrations.ErrInvalidTransactionMode},
		{name: "empty up", version: 1, migration: "create_users", targetError: migrations.ErrEmptyUpSQL},
		{name: "whitespace down", version: 1, migration: "create_users", upSQL: "SELECT 1;", downSQL: " \n", targetError: migrations.ErrInvalidFormat},
		{name: "invalid UTF-8", version: 1, migration: "create_users", upSQL: string([]byte{0xff}), targetError: migrations.ErrInvalidEncoding},
		{name: "NUL down SQL", version: 1, migration: "create_users", upSQL: "SELECT 1;", downSQL: "SELECT \x00;", targetError: migrations.ErrInvalidEncoding},
		{name: "oversized SQL", version: 1, migration: "create_users", upSQL: strings.Repeat("x", (16<<20)+1), targetError: migrations.ErrInvalidEncoding},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := migrations.NewMigration(
				test.version,
				test.migration,
				test.mode,
				test.upSQL,
				test.downSQL,
			)
			if !errors.Is(err, test.targetError) {
				t.Fatalf("NewMigration() error = %v, want %v", err, test.targetError)
			}
		})
	}
}

func TestParseChecksumRoundTripsCanonicalText(t *testing.T) {
	t.Parallel()

	const encoded = "sha256:74ec4f716c2502dabb1388ec2d41313ed04fed35729dd9221feb5d5972535801"

	checksum, err := migrations.ParseChecksum(encoded)
	if err != nil {
		t.Fatalf("ParseChecksum() error = %v", err)
	}
	if checksum.String() != encoded {
		t.Fatalf("String() = %q, want %q", checksum, encoded)
	}
	if _, err := migrations.ParseChecksum("md5:971d"); !errors.Is(err, migrations.ErrInvalidChecksum) {
		t.Fatalf("ParseChecksum(invalid) error = %v, want ErrInvalidChecksum", err)
	}
	if _, err := migrations.ParseChecksum("sha256:" + strings.Repeat("0", 64)); !errors.Is(err, migrations.ErrInvalidChecksum) {
		t.Fatalf("ParseChecksum(zero) error = %v, want ErrInvalidChecksum", err)
	}
}

func TestMigrationChecksumCannotShiftSQLAcrossSectionBoundary(t *testing.T) {
	t.Parallel()

	first, err := migrations.NewMigration(
		1,
		"boundary",
		migrations.TransactionModeDefault,
		"SELECT 1;\n",
		"DROP TABLE first;\n-- down --\nDROP TABLE second;\n",
	)
	if err != nil {
		t.Fatalf("NewMigration(first) error = %v", err)
	}
	second, err := migrations.NewMigration(
		1,
		"boundary",
		migrations.TransactionModeDefault,
		"SELECT 1;\n-- down --\nDROP TABLE first;\n",
		"DROP TABLE second;\n",
	)
	if err != nil {
		t.Fatalf("NewMigration(second) error = %v", err)
	}
	if first.Checksum() == second.Checksum() {
		t.Fatalf("distinct section boundaries produced checksum %s", first.Checksum())
	}
}
