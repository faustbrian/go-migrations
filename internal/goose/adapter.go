// Package goose contains the replaceable, hidden Goose execution adapter pinned
// by go.mod. It translates validated migrations without parsing source files or
// owning ledger history. Transactions remain caller-owned; no-transaction SQL
// runs directly on the leased lock connection rather than an unrelated pool
// connection. Execution errors retain redacted cause trees and classifications.
// See adapter_internal_test.go for the lock-connection bypass guard.
package goose

import (
	"context"
	"database/sql"
	"errors"

	migrations "github.com/faustbrian/go-migrations/v4"
	pressly "github.com/pressly/goose/v3"
)

// ErrUnsupportedMigration indicates a canonical migration the hidden adapter
// cannot safely represent.
var ErrUnsupportedMigration = errors.New("migration is unsupported by execution adapter")

type executionError struct {
	cause error
}

// Error reports the stable execution classification, never SQL or driver text.
func (*executionError) Error() string { return migrations.ErrExecutionFailed.Error() }

// Is exposes the execution category without inventing another failure cause.
func (*executionError) Is(target error) bool { return target == migrations.ErrExecutionFailed }

// Unwrap retains context causes and redacted independent failures, never the
// original SQL or driver diagnostic.
func (err *executionError) Unwrap() error {
	return err.cause
}

func redactExecutionError(cause error) error {
	return &executionError{cause: redactExecutionCause(cause)}
}

func redactExecutionCause(cause error) error {
	// Inspect every joined branch before testing context identity: errors.Is on
	// the aggregate also matches cancellation joined with an independent failure.
	if joined, ok := cause.(interface{ Unwrap() []error }); ok {
		var redacted []error
		for _, child := range joined.Unwrap() {
			if child != nil {
				redacted = append(redacted, redactExecutionCause(child))
			}
		}
		if len(redacted) > 0 {
			return retainExecutionContext(cause, errors.Join(redacted...))
		}
	}
	if child := errors.Unwrap(cause); child != nil {
		return retainExecutionContext(cause, redactExecutionCause(child))
	}

	switch {
	case errors.Is(cause, context.Canceled) && errors.Is(cause, context.DeadlineExceeded):
		return errors.Join(context.Canceled, context.DeadlineExceeded)
	case errors.Is(cause, context.Canceled):
		return context.Canceled
	case errors.Is(cause, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return migrations.ErrExecutionFailed
	}
}

func retainExecutionContext(cause, redacted error) error {
	// A driver may implement its context classification on a wrapper's Is
	// method, independently of its unwrapped cause. Preserve that match without
	// retaining the wrapper or replacing any independent redacted failures.
	matcher, ok := cause.(interface{ Is(error) bool })
	if !ok {
		return redacted
	}
	for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
		if matcher.Is(target) && !errors.Is(redacted, target) {
			redacted = errors.Join(redacted, target)
		}
	}
	return redacted
}

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
	// Migration owns validation through private fields; its zero value is the
	// only invalid value callers can construct without NewMigration.
	if migration.Version() == 0 {
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

	compiled := pressly.NewGoMigration(gooseVersion(migration), up, down)

	return &Adapter{
		migration: compiled,
		upSQL:     migration.UpSQL(),
		downSQL:   migration.DownSQL(),
		mode:      migration.TransactionMode(),
	}, nil
}

func gooseVersion(migration migrations.Migration) int64 {
	// #nosec G115 -- Migration has private fields and NewMigration rejects
	// versions above math.MaxInt64 before this internal adapter can receive one.
	return int64(migration.Version())
}

// RollbackTx executes transactional down SQL through Goose's hidden function.
func (adapter *Adapter) RollbackTx(ctx context.Context, tx *sql.Tx) error {
	if adapter == nil || adapter.migration == nil || tx == nil ||
		adapter.mode != migrations.TransactionModeDefault ||
		adapter.migration.DownFnContext == nil {
		return ErrUnsupportedMigration
	}

	if err := adapter.migration.DownFnContext(ctx, tx); err != nil {
		return redactExecutionError(err)
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
		return redactExecutionError(err)
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
		return redactExecutionError(err)
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
		return redactExecutionError(err)
	}

	return nil
}
