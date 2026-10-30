package postgres

import (
	"context"
	"errors"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

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

// executeMigrationSQL runs caller-reviewed migration SQL on the caller-owned
// transaction or advisory-lock connection, never on another pool session.
// Exec without arguments uses the simple protocol, so a migration file may
// contain several statements.
func executeMigrationSQL(ctx context.Context, execer queryer, statement string) error {
	if _, err := execer.Exec(ctx, statement); err != nil {
		// A pool using pgconn's CancelRequestContextWatcherHandler reports the
		// caller's cancellation as SQLSTATE 57014 query_canceled rather than a
		// context error. With the context already done that is the caller's
		// cancellation; with a live context 57014 means statement_timeout.
		var pgErr *pgconn.PgError
		if ctxErr := ctx.Err(); ctxErr != nil && errors.As(err, &pgErr) && pgErr.Code == queryCanceledCode {
			return &executionError{cause: ctxErr}
		}
		return redactExecutionError(err)
	}

	return nil
}

// queryCanceledCode is PostgreSQL's SQLSTATE for a canceled statement
// (https://www.postgresql.org/docs/current/errcodes-appendix.html).
const queryCanceledCode = "57014"

// A zero Migration is the only invalid value callers can construct without
// NewMigration, which owns validation through private fields.
func validMigration(migration migrations.Migration) error {
	if migration.Version() == 0 {
		return migrations.ErrInvalidVersion
	}

	return nil
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
