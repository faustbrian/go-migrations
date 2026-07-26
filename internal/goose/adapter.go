// Package goose contains the replaceable, hidden Goose execution adapter.
package goose

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	migrations "github.com/faustbrian/golib/pkg/migrations"
	pressly "github.com/pressly/goose/v3"
)

// ErrUnsupportedMigration indicates a canonical migration the hidden adapter
// cannot safely represent.
var ErrUnsupportedMigration = errors.New("migration is unsupported by execution adapter")

// Adapter contains a Goose migration without exposing Goose from this internal
// package.
type Adapter struct {
	migration *pressly.Migration
	upSQL     string
	downSQL   string
	mode      migrations.TransactionMode
}

// Compile translates the owned migration contract to Goose's Go-migration
// execution shape. Goose never parses canonical files or owns version state.
func Compile(migration migrations.Migration) (*Adapter, error) {
	if migration.Version() == 0 ||
		migration.Name() == "" ||
		migration.Checksum() == (migrations.Checksum{}) ||
		strings.TrimSpace(migration.UpSQL()) == "" ||
		(migration.TransactionMode() != migrations.TransactionModeDefault &&
			migration.TransactionMode() != migrations.TransactionModeNone) {
		return nil, ErrUnsupportedMigration
	}

	var up *pressly.GoFunc
	var down *pressly.GoFunc
	if migration.TransactionMode() == migrations.TransactionModeDefault {
		up = &pressly.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, migration.UpSQL())

			return err
		}}
		if migration.DownSQL() != "" {
			down = &pressly.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, migration.DownSQL())

				return err
			}}
		}
	} else if migration.TransactionMode() == migrations.TransactionModeNone {
		up = &pressly.GoFunc{RunDB: func(context.Context, *sql.DB) error {
			return ErrUnsupportedMigration
		}}
		if migration.DownSQL() != "" {
			down = &pressly.GoFunc{RunDB: func(context.Context, *sql.DB) error {
				return ErrUnsupportedMigration
			}}
		}
	}

	compiled := pressly.NewGoMigration(int64(migration.Version()), up, down)

	return &Adapter{
		migration: compiled,
		upSQL:     migration.UpSQL(),
		downSQL:   migration.DownSQL(),
		mode:      migration.TransactionMode(),
	}, nil
}

// RollbackTx executes transactional down SQL through Goose's hidden function.
func (adapter *Adapter) RollbackTx(ctx context.Context, tx *sql.Tx) error {
	if adapter == nil || adapter.migration == nil || tx == nil ||
		adapter.mode != migrations.TransactionModeDefault ||
		adapter.migration.DownFnContext == nil {
		return ErrUnsupportedMigration
	}

	if err := adapter.migration.DownFnContext(ctx, tx); err != nil {
		return fmt.Errorf("execute transactional down SQL: %w", err)
	}

	return nil
}

// ApplyTx executes a transactional canonical migration through Goose's hidden
// Go-migration function on the caller-owned transaction.
func (adapter *Adapter) ApplyTx(ctx context.Context, tx *sql.Tx) error {
	if adapter == nil || adapter.migration == nil || tx == nil ||
		adapter.mode != migrations.TransactionModeDefault ||
		adapter.migration.UpFnContext == nil {
		return ErrUnsupportedMigration
	}

	if err := adapter.migration.UpFnContext(ctx, tx); err != nil {
		return fmt.Errorf("execute transactional up SQL: %w", err)
	}

	return nil
}

// ApplyConn executes an explicit no-transaction migration on the caller-owned
// lock connection. Goose's no-transaction function accepts only *sql.DB, so
// direct connection execution preserves the stronger lock-lifetime contract.
func (adapter *Adapter) ApplyConn(ctx context.Context, connection *sql.Conn) error {
	if adapter == nil || adapter.migration == nil || connection == nil ||
		adapter.mode != migrations.TransactionModeNone {
		return ErrUnsupportedMigration
	}

	if _, err := connection.ExecContext(ctx, adapter.upSQL); err != nil {
		return fmt.Errorf("execute no-transaction up SQL: %w", err)
	}

	return nil
}

// RollbackConn executes explicit no-transaction down SQL while retaining the
// caller-owned advisory-lock connection.
func (adapter *Adapter) RollbackConn(ctx context.Context, connection *sql.Conn) error {
	if adapter == nil || adapter.migration == nil || connection == nil ||
		adapter.mode != migrations.TransactionModeNone || adapter.downSQL == "" {
		return ErrUnsupportedMigration
	}

	if _, err := connection.ExecContext(ctx, adapter.downSQL); err != nil {
		return fmt.Errorf("execute no-transaction down SQL: %w", err)
	}

	return nil
}
