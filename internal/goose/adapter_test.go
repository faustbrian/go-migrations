package goose_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	migrations "github.com/faustbrian/go-migrations/v3"
	gooseadapter "github.com/faustbrian/go-migrations/v3/internal/goose"
)

func TestAdapterExecutesTransactionalMigrationOnCallerTransaction(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migration := adapterMigration(t, migrations.TransactionModeDefault)
	adapter, err := gooseadapter.Compile(migration)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	mock.ExpectBegin()
	transaction, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	if err := adapter.ApplyTx(context.Background(), transaction); err != nil {
		t.Fatalf("ApplyTx() error = %v", err)
	}
	if err := adapter.RollbackTx(context.Background(), transaction); err != nil {
		t.Fatalf("RollbackTx() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestAdapterExecutesNoTransactionMigrationOnCallerConnection(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	connection, err := database.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn() error = %v", err)
	}
	defer func() { _ = connection.Close() }()
	migration := adapterMigration(t, migrations.TransactionModeNone)
	adapter, err := gooseadapter.Compile(migration)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(migration.UpSQL())).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(migration.DownSQL())).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := adapter.ApplyConn(context.Background(), connection); err != nil {
		t.Fatalf("ApplyConn() error = %v", err)
	}
	if err := adapter.RollbackConn(context.Background(), connection); err != nil {
		t.Fatalf("RollbackConn() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func TestAdapterRejectsInvalidMigrationAndExecutionMode(t *testing.T) {
	t.Parallel()

	if _, err := gooseadapter.Compile(migrations.Migration{}); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("Compile(zero) error = %v, want ErrUnsupportedMigration", err)
	}
	var adapter *gooseadapter.Adapter
	if err := adapter.ApplyTx(context.Background(), nil); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("ApplyTx(nil) error = %v", err)
	}
}

func TestAdapterRedactsExecutionFailuresAndRejectsInvalidCalls(t *testing.T) {
	t.Parallel()

	fault := errors.New("statement failed: SELECT private_payload")
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	connection, err := database.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn() error = %v", err)
	}
	defer func() { _ = connection.Close() }()

	transactional := adapterMigration(t, migrations.TransactionModeDefault)
	txAdapter, err := gooseadapter.Compile(transactional)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	mock.ExpectBegin()
	transaction, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(transactional.UpSQL())).WillReturnError(fault)
	assertExecutionErrorRedacted(t, txAdapter.ApplyTx(context.Background(), transaction), fault)
	mock.ExpectExec(regexp.QuoteMeta(transactional.DownSQL())).WillReturnError(fault)
	assertExecutionErrorRedacted(t, txAdapter.RollbackTx(context.Background(), transaction), fault)
	deadlineFault := fmt.Errorf("private deadline detail: %w", context.DeadlineExceeded)
	mock.ExpectExec(regexp.QuoteMeta(transactional.UpSQL())).WillReturnError(deadlineFault)
	deadlineErr := txAdapter.ApplyTx(context.Background(), transaction)
	assertExecutionErrorRedacted(t, deadlineErr, deadlineFault)
	if !errors.Is(deadlineErr, context.DeadlineExceeded) {
		t.Fatalf("ApplyTx(deadline) error = %v, want context deadline", deadlineErr)
	}
	canceledFault := fmt.Errorf("private cancellation detail: %w", context.Canceled)
	mock.ExpectExec(regexp.QuoteMeta(transactional.DownSQL())).WillReturnError(canceledFault)
	canceledErr := txAdapter.RollbackTx(context.Background(), transaction)
	assertExecutionErrorRedacted(t, canceledErr, canceledFault)
	if !errors.Is(canceledErr, context.Canceled) {
		t.Fatalf("RollbackTx(canceled) error = %v, want context cancellation", canceledErr)
	}
	mock.ExpectRollback()
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if err := txAdapter.ApplyConn(context.Background(), connection); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("ApplyConn(transactional) error = %v", err)
	}

	noTx := adapterMigration(t, migrations.TransactionModeNone)
	noTxAdapter, err := gooseadapter.Compile(noTx)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(noTx.UpSQL())).WillReturnError(fault)
	assertExecutionErrorRedacted(t, noTxAdapter.ApplyConn(context.Background(), connection), fault)
	mock.ExpectExec(regexp.QuoteMeta(noTx.DownSQL())).WillReturnError(fault)
	assertExecutionErrorRedacted(t, noTxAdapter.RollbackConn(context.Background(), connection), fault)
	if err := noTxAdapter.ApplyTx(context.Background(), transaction); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("ApplyTx(no-tx) error = %v", err)
	}

	irreversible, err := migrations.NewMigration(2, "irreversible", migrations.TransactionModeDefault, "SELECT 1;", "")
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}
	irreversibleAdapter, err := gooseadapter.Compile(irreversible)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if err := irreversibleAdapter.RollbackTx(context.Background(), transaction); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("RollbackTx(irreversible) error = %v", err)
	}
	if err := noTxAdapter.RollbackTx(context.Background(), transaction); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("RollbackTx(no-tx) error = %v", err)
	}
	if err := txAdapter.RollbackConn(context.Background(), connection); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("RollbackConn(transactional) error = %v", err)
	}

	var nilAdapter *gooseadapter.Adapter
	if err := nilAdapter.RollbackTx(context.Background(), transaction); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("nil RollbackTx() error = %v", err)
	}
	if err := nilAdapter.ApplyConn(context.Background(), connection); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("nil ApplyConn() error = %v", err)
	}
	if err := nilAdapter.RollbackConn(context.Background(), connection); !errors.Is(err, gooseadapter.ErrUnsupportedMigration) {
		t.Fatalf("nil RollbackConn() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database expectations: %v", err)
	}
}

func assertExecutionErrorRedacted(t *testing.T, err error, sensitive error) {
	t.Helper()

	if !errors.Is(err, migrations.ErrExecutionFailed) {
		t.Fatalf("execution error = %v, want ErrExecutionFailed", err)
	}
	if errors.Is(err, sensitive) {
		t.Fatal("execution error retained the sensitive driver cause")
	}
	if strings.Contains(err.Error(), sensitive.Error()) || strings.Contains(err.Error(), "private_payload") {
		t.Fatalf("execution error disclosed driver detail: %q", err)
	}
}

func adapterMigration(
	t *testing.T,
	mode migrations.TransactionMode,
) migrations.Migration {
	t.Helper()

	migration, err := migrations.NewMigration(
		1,
		"create_users",
		mode,
		"CREATE TABLE users (id bigint);",
		"DROP TABLE users;",
	)
	if err != nil {
		t.Fatalf("NewMigration() error = %v", err)
	}

	return migration
}

func TestAdapterPreservesCancellationOnlyVersusIndependentFailure(t *testing.T) {
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
	for _, mode := range []migrations.TransactionMode{migrations.TransactionModeDefault, migrations.TransactionModeNone} {
		for _, rollback := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("mode=%v/rollback=%v/%s", mode, rollback, tc.name), func(t *testing.T) {
					db, mock, err := sqlmock.New()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = db.Close() })
					migration := adapterMigration(t, mode)
					adapter, err := gooseadapter.Compile(migration)
					if err != nil {
						t.Fatal(err)
					}
					sql := migration.UpSQL()
					if rollback {
						sql = migration.DownSQL()
					}
					var got error
					if mode == migrations.TransactionModeDefault {
						mock.ExpectBegin()
						tx, err := db.BeginTx(context.Background(), nil)
						if err != nil {
							t.Fatal(err)
						}
						mock.ExpectExec(regexp.QuoteMeta(sql)).WillReturnError(tc.cause)
						if rollback {
							got = adapter.RollbackTx(context.Background(), tx)
						} else {
							got = adapter.ApplyTx(context.Background(), tx)
						}
						mock.ExpectRollback()
						if err := tx.Rollback(); err != nil {
							t.Fatal(err)
						}
					} else {
						conn, err := db.Conn(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = conn.Close() }()
						mock.ExpectExec(regexp.QuoteMeta(sql)).WillReturnError(tc.cause)
						if rollback {
							got = adapter.RollbackConn(context.Background(), conn)
						} else {
							got = adapter.ApplyConn(context.Background(), conn)
						}
					}
					if onlyContextCauses(got) != tc.onlyContext {
						t.Errorf("context-only classification = %v, want %v", onlyContextCauses(got), tc.onlyContext)
					}
					for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
						if errors.Is(got, target) != errors.Is(tc.cause, target) {
							t.Errorf("lost context classification %v", target)
						}
					}
					assertExecutionErrorRedacted(t, got, private)
					for _, format := range []string{"%s", "%q", "%v", "%+v", "%#v"} {
						if strings.Contains(fmt.Sprintf(format, got), "private") {
							t.Errorf("format %s exposed private diagnostics", format)
						}
					}
					if err := mock.ExpectationsWereMet(); err != nil {
						t.Fatal(err)
					}
				})
			}
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
