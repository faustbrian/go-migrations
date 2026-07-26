package goose

import (
	"context"
	"errors"
	"testing"

	migrations "github.com/faustbrian/golib/pkg/migrations"
)

func TestNoTransactionGooseFunctionsCannotBypassLockConnection(t *testing.T) {
	t.Parallel()

	migration, err := migrations.NewMigration(
		1,
		"no_transaction",
		migrations.TransactionModeNone,
		"SELECT 1;",
		"SELECT 2;",
	)
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}
	adapter, err := Compile(migration)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if err := adapter.migration.UpFnNoTxContext(context.Background(), nil); !errors.Is(err, ErrUnsupportedMigration) {
		t.Fatalf("Goose up bypass error = %v", err)
	}
	if err := adapter.migration.DownFnNoTxContext(context.Background(), nil); !errors.Is(err, ErrUnsupportedMigration) {
		t.Fatalf("Goose down bypass error = %v", err)
	}
}
