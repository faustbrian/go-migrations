package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/jackc/pgx/v5"
)

func TestMigrationSQLRunsOnTheCallerOwnedExecutor(t *testing.T) {
	t.Parallel()

	const statement = "CREATE TABLE users (id bigint); CREATE INDEX users_id ON users (id);"
	mock := NewMockConn()
	mock.ExpectExec(regexp.QuoteMeta(statement)).WithArgs().WillReturnResult(MockResult("CREATE INDEX", 0))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(statement)).WithArgs().WillReturnResult(MockResult("CREATE INDEX", 0))
	mock.ExpectRollback()

	if err := executeMigrationSQL(context.Background(), mock, statement); err != nil {
		t.Fatalf("connection execution error = %v", err)
	}
	tx, err := mock.begin(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := executeMigrationSQL(context.Background(), tx, statement); err != nil {
		t.Fatalf("transaction execution error = %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRejectsZeroMigrationBeforeDatabaseWork(t *testing.T) {
	t.Parallel()

	session, mock := faultSession(t, 0)
	if _, err := session.Apply(context.Background(), migrations.Migration{}); !errors.Is(err, migrations.ErrInvalidVersion) {
		t.Fatalf("Apply(zero) error = %v, want ErrInvalidVersion", err)
	}
	if _, err := session.Rollback(context.Background(), migrations.Migration{}); !errors.Is(err, migrations.ErrInvalidVersion) {
		t.Fatalf("Rollback(zero) error = %v, want ErrInvalidVersion", err)
	}
	assertFaultExpectations(t, mock)
}

func TestExecutionPreservesCancellationOnlyVersusIndependentFailure(t *testing.T) {
	t.Parallel()

	private := errors.New("private execution diagnostic")
	cases := []struct {
		name        string
		cause       error
		onlyContext bool
	}{
		{"canceled", context.Canceled, true},
		{"deadline", context.DeadlineExceeded, true},
		{"wrapped cancellation", fmt.Errorf("private wrapper: %w", context.Canceled), true},
		{"both context causes", errors.Join(context.Canceled, context.DeadlineExceeded), true},
		{"canceled and failure", errors.Join(context.Canceled, private), false},
		{"deadline and failure", errors.Join(context.DeadlineExceeded, private), false},
		{"wrapped mixed causes", fmt.Errorf("private wrapper: %w", errors.Join(context.Canceled, private)), false},
		{"ordinary failure", private, false},
		{"custom cancellation with failure", classifiedDriverError{cause: private, target: context.Canceled}, false},
		{"custom deadline with failure", classifiedDriverError{cause: private, target: context.DeadlineExceeded}, false},
		{"custom cancellation with deadline", classifiedDriverError{cause: context.DeadlineExceeded, target: context.Canceled}, true},
		{"custom cancellation leaf", classifiedDriverError{target: context.Canceled}, true},
		{"custom cancellation with joined failures", classifiedJoinedDriverError{causes: []error{context.DeadlineExceeded, private}, target: context.Canceled}, false},
		{"custom deadline with joined context", classifiedJoinedDriverError{causes: []error{context.Canceled}, target: context.DeadlineExceeded}, true},
	}
	for _, transactional := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("transaction=%t/%s", transactional, tc.name), func(t *testing.T) {
				t.Parallel()
				const statement = "DROP TABLE private_payload;"
				mock := NewMockConn()
				var execer queryer = mock
				if transactional {
					mock.ExpectBegin()
					tx, err := mock.begin(context.Background(), pgx.TxOptions{})
					if err != nil {
						t.Fatal(err)
					}
					execer = tx
				}
				mock.ExpectExec(regexp.QuoteMeta(statement)).WillReturnError(tc.cause)
				got := executeMigrationSQL(context.Background(), execer, statement)
				if onlyContextCauses(got) != tc.onlyContext {
					t.Errorf("context-only classification = %v, want %v", onlyContextCauses(got), tc.onlyContext)
				}
				for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
					if errors.Is(got, target) != errors.Is(tc.cause, target) {
						t.Errorf("lost context classification %v", target)
					}
				}
				if !errors.Is(got, migrations.ErrExecutionFailed) || errors.Is(got, private) {
					t.Errorf("execution error = %v, want redacted ErrExecutionFailed", got)
				}
				for _, format := range []string{"%s", "%q", "%v", "%+v", "%#v"} {
					if rendered := fmt.Sprintf(format, got); strings.Contains(rendered, "private") {
						t.Errorf("format %s exposed private diagnostics: %s", format, rendered)
					}
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type classifiedDriverError struct {
	cause  error
	target error
}

func (classifiedDriverError) Error() string { return "private classified driver diagnostic" }

func (err classifiedDriverError) Unwrap() error { return err.cause }

func (err classifiedDriverError) Is(target error) bool { return target == err.target }

type classifiedJoinedDriverError struct {
	causes []error
	target error
}

func (classifiedJoinedDriverError) Error() string { return "private joined driver diagnostic" }

func (err classifiedJoinedDriverError) Unwrap() []error { return err.causes }

func (err classifiedJoinedDriverError) Is(target error) bool { return target == err.target }

// This consumer-style classifier visits every terminal cause; errors.Is alone
// cannot distinguish cancellation from cancellation joined with a real failure.
func onlyContextCauses(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyContextCauses(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return onlyContextCauses(cause)
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}
