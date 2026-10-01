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
