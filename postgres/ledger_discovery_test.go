package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	migrations "github.com/faustbrian/go-migrations/v3"
	"github.com/faustbrian/go-migrations/v3/postgres"
)

// MIG-04: discovery must not acquire migration ownership or prepare a ledger.
// The mock deliberately admits only a read transaction and catalog/history IO;
// real PostgreSQL read-only enforcement is exercised by the integration lane.
func TestReadLedgerDoesNotPrepareFreshDatabase(t *testing.T) {
	db, mock := newMockDatabase(t)
	backend, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := any(backend).(interface {
		ReadLedger(context.Context) ([]migrations.Record, error)
	})
	if !ok {
		t.Fatal("native backend cannot discover ledger without preparation")
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*relkind").WillReturnRows(sqlmock.NewRows([]string{"relkind"}))
	mock.ExpectRollback()
	records, err := reader.ReadLedger(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatal("fresh read-only discovery failed", err)
	}
	assertExpectations(t, mock)
}

func TestReadLedgerFailuresAreBoundedRedactedAndReleaseTransactions(t *testing.T) {
	for _, failure := range []string{"begin", "catalog", "records", "decode", "iterate", "rollback", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			db, mock := newMockDatabase(t)
			backend, err := postgres.New(db, postgres.WithOperationTimeout(10*time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("private-diagnostic")
			begin := mock.ExpectBegin()
			if failure == "begin" {
				begin.WillReturnError(cause)
			} else {
				catalog := mock.ExpectQuery("SELECT.*relkind")
				switch failure {
				case "catalog":
					catalog.WillReturnError(cause)
				case "timeout":
					catalog.WillDelayFor(time.Second).WillReturnRows(sqlmock.NewRows([]string{"relkind"}))
				default:
					catalog.WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow("r"))
					history := mock.ExpectQuery("SELECT.*FROM public.migrations")
					if failure == "records" {
						history.WillReturnError(cause)
					} else {
						rows := sqlmock.NewRows([]string{"kind", "version", "name", "checksum", "started_at", "finished_at", "execution_time_ms", "dirty"})
						if failure == "decode" {
							rows.AddRow("migration", 1, "owned", "invalid", time.Unix(1, 0).UTC(), nil, 0, true)
						}
						if failure == "iterate" {
							rows.AddRow("migration", 1, "owned", migrations.ChecksumData([]byte("owned")).String(), time.Unix(1, 0).UTC(), nil, 0, true).RowError(0, cause)
						}
						history.WillReturnRows(rows)
					}
				}
				if failure != "timeout" {
					rollback := mock.ExpectRollback()
					if failure == "rollback" {
						rollback.WillReturnError(cause)
					}
				}
			}
			records, err := backend.ReadLedger(context.Background())
			if err == nil || len(records) != 0 || strings.Contains(err.Error(), cause.Error()) {
				t.Fatal("discovery failure lost denial/redaction")
			}
			if failure == "timeout" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("discovery did not preserve deadline")
				}
			} else {
				assertExpectations(t, mock)
			}
		})
	}
}

func TestReadLedgerRejectsInvalidOrCanceledOwnersBeforeIO(t *testing.T) {
	var absent *postgres.Backend
	if _, err := absent.ReadLedger(context.Background()); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatal("nil owner admitted")
	}
	db, mock := newMockDatabase(t)
	backend, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	//lint:ignore SA1012 Exercise the documented nil-context rejection before IO.
	//nolint:staticcheck // SA1012: exercise the documented nil-context rejection.
	if _, err := backend.ReadLedger(nil); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatal("nil context admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.ReadLedger(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled context lost")
	}
	assertExpectations(t, mock)
}

func TestReadLedgerPreservesDirtyHistoryAndRejectsForeignRelation(t *testing.T) {
	for _, kind := range []string{"r", "v"} {
		t.Run(kind, func(t *testing.T) {
			db, mock := newMockDatabase(t)
			backend, err := postgres.New(db)
			if err != nil {
				t.Fatal(err)
			}
			reader, ok := any(backend).(interface {
				ReadLedger(context.Context) ([]migrations.Record, error)
			})
			if !ok {
				t.Fatal("native backend lacks read-only discovery")
			}
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT.*relkind").WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow(kind))
			if kind == "r" {
				mock.ExpectQuery("SELECT.*FROM public.migrations").WillReturnRows(sqlmock.NewRows([]string{"kind", "version", "name", "checksum", "started_at", "finished_at", "execution_time_ms", "dirty"}).AddRow("migration", 1, "owned_schema", migrations.ChecksumData([]byte("schema")).String(), time.Unix(1, 0).UTC(), nil, 0, true))
			}
			mock.ExpectRollback()
			records, err := reader.ReadLedger(context.Background())
			if kind == "r" {
				if err != nil || len(records) != 1 || !records[0].Dirty() {
					t.Fatal("dirty history was lost", err)
				}
			} else if !errors.Is(err, postgres.ErrLedgerConflict) || len(records) != 0 {
				t.Fatal("foreign relation admitted", err)
			}
			assertExpectations(t, mock)
		})
	}
}
