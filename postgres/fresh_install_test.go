package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v5"
	"github.com/faustbrian/go-migrations/v5/postgres"
)

type freshInstaller interface {
	InstallFresh(context.Context, []migrations.Migration, string) ([]migrations.Record, error)
}

func TestInstallFreshRejectsInvalidInputWithoutIO(t *testing.T) {
	db, mock := newMockDatabase(t)
	backend, _ := postgres.NewMockBackend(db)
	first, _ := migrations.NewMigration(1, "first", migrations.TransactionModeDefault, "SELECT 1", "")
	nontransactional, _ := migrations.NewMigration(2, "second", migrations.TransactionModeNone, "SELECT 2", "")
	for _, tc := range []struct {
		name      string
		history   []migrations.Migration
		preflight string
	}{
		{"empty", nil, ""},
		{"zero", []migrations.Migration{{}}, ""},
		{"duplicate", []migrations.Migration{first, first}, ""},
		{"no_transaction", []migrations.Migration{first, nontransactional}, ""},
		{"count", make([]migrations.Migration, migrations.MaxMigrationFiles+1), ""},
		{"nul", []migrations.Migration{first}, "SELECT \x00"},
		{"encoding", []migrations.Migration{first}, "\xff"},
		{"blank", []migrations.Migration{first}, "  \n"},
		{"bytes", []migrations.Migration{first}, strings.Repeat("x", migrations.MaxMigrationSourceBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if records, err := backend.InstallFresh(context.Background(), tc.history, tc.preflight); err == nil || len(records) != 0 {
				t.Fatal("invalid install input admitted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.InstallFresh(ctx, []migrations.Migration{first}, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled install admitted", err)
	}
	//lint:ignore SA1012 Exercise the documented nil-context rejection before IO.
	//nolint:staticcheck // SA1012: exercise the documented nil-context rejection.
	if _, err := backend.InstallFresh(nil, []migrations.Migration{first}, ""); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatal("nil context admitted", err)
	}
	var absent *postgres.Backend
	if _, err := absent.InstallFresh(context.Background(), []migrations.Migration{first}, ""); !errors.Is(err, postgres.ErrInvalidConfig) {
		t.Fatal("nil owner admitted", err)
	}
	assertExpectations(t, mock)
}

func TestInstallFreshCommitAndReleaseFailuresRemainUnknown(t *testing.T) {
	for _, failure := range []string{"", "commit", "unlock", "preflight", "existing"} {
		t.Run(failure, func(t *testing.T) {
			db, mock := newMockDatabase(t)
			backend, _ := postgres.NewMockBackend(db)
			step, _ := migrations.NewMigration(1, "owned", migrations.TransactionModeDefault, "SELECT 1", "")
			mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(postgres.NewMockRows([]string{"locked"}).AddRow(true))
			mock.ExpectBegin()
			mock.ExpectExec("SELECT set_config").WillReturnResult(postgres.MockResult("SELECT", 1))
			mock.ExpectExec("CREATE TABLE IF NOT EXISTS public.migrations").WillReturnResult(postgres.MockResult("SELECT", 0))
			rows := postgres.NewMockRows([]string{"kind", "version", "name", "checksum", "started_at", "finished_at", "execution_time_ms", "dirty"})
			if failure == "existing" {
				rows.AddRow("migration", 1, step.Name(), step.Checksum().String(), time.Unix(1, 0), time.Unix(2, 0), 1000, false)
			}
			mock.ExpectQuery("SELECT.*FROM public.migrations").WillReturnRows(rows)
			cause := errors.New("private diagnostic")
			if failure != "existing" {
				setup := mock.ExpectExec("SELECT 42")
				if failure == "preflight" {
					setup.WillReturnError(cause)
				} else {
					setup.WillReturnResult(postgres.MockResult("SELECT", 1))
					mock.ExpectExec("INSERT INTO public.migrations").WillReturnResult(postgres.MockResult("SELECT", 1))
					mock.ExpectExec("SELECT 1").WillReturnResult(postgres.MockResult("SELECT", 1))
					mock.ExpectExec("UPDATE public.migrations").WillReturnResult(postgres.MockResult("SELECT", 1))
				}
			}
			if failure == "existing" || failure == "preflight" {
				mock.ExpectRollback()
			} else {
				commit := mock.ExpectCommit()
				if failure == "commit" {
					commit.WillReturnError(cause)
				}
			}
			unlock := mock.ExpectQuery("SELECT pg_advisory_unlock")
			if failure == "unlock" {
				unlock.WillReturnError(cause)
			} else {
				unlock.WillReturnRows(postgres.NewMockRows([]string{"unlocked"}).AddRow(true))
			}
			records, err := backend.InstallFresh(context.Background(), []migrations.Migration{step}, "SELECT 42")
			if failure == "" {
				if err != nil || len(records) != 1 || records[0].Checksum() != step.Checksum() || records[0].Dirty() {
					t.Fatal("fresh native identity lost", err)
				}
			} else {
				if err == nil || len(records) != 0 || strings.Contains(err.Error(), cause.Error()) {
					t.Fatal("failed install escaped denial/redaction")
				}
				if unknown := errors.Is(err, postgres.ErrInstallOutcomeUnknown); unknown != (failure == "commit" || failure == "unlock") {
					t.Fatal("install ambiguity misclassified", err)
				}
				if failure == "existing" && !errors.Is(err, postgres.ErrLedgerConflict) {
					t.Fatal("existing history reclassified", err)
				}
			}
			assertExpectations(t, mock)
		})
	}
}

// MIG-02: a late source failure must roll back setup and all earlier steps,
// rather than leaving the preflight and first migration committed.
func TestInstallFreshRollsBackCompleteInstallation(t *testing.T) {
	db, mock := newMockDatabase(t)
	backend, err := postgres.NewMockBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	installer, ok := any(backend).(freshInstaller)
	if !ok {
		t.Fatal("native backend cannot install preflight and full history atomically")
	}
	first, _ := migrations.NewMigration(1, "first", migrations.TransactionModeDefault, "SELECT 1", "")
	second, _ := migrations.NewMigration(2, "second", migrations.TransactionModeDefault, "SELECT 2", "")
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs(postgres.MockAnyArg()).WillReturnRows(postgres.NewMockRows([]string{"locked"}).AddRow(true))
	mock.ExpectBegin()
	mock.ExpectExec("SELECT set_config").WillReturnResult(postgres.MockResult("SELECT", 1))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS public.migrations").WillReturnResult(postgres.MockResult("SELECT", 0))
	mock.ExpectQuery("SELECT.*FROM public.migrations").WillReturnRows(postgres.NewMockRows([]string{"kind", "version", "name", "checksum", "started_at", "finished_at", "execution_time_ms", "dirty"}))
	mock.ExpectExec("SELECT 42").WillReturnResult(postgres.MockResult("SELECT", 1))
	mock.ExpectExec("INSERT INTO public.migrations").WillReturnResult(postgres.MockResult("SELECT", 1))
	mock.ExpectExec("SELECT 1").WillReturnResult(postgres.MockResult("SELECT", 1))
	mock.ExpectExec("UPDATE public.migrations").WillReturnResult(postgres.MockResult("SELECT", 1))
	mock.ExpectExec("INSERT INTO public.migrations").WillReturnResult(postgres.MockResult("SELECT", 1))
	mock.ExpectExec("SELECT 2").WillReturnError(errors.New("private SQL diagnostic"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT pg_advisory_unlock").WithArgs(postgres.MockAnyArg()).WillReturnRows(postgres.NewMockRows([]string{"unlocked"}).AddRow(true))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	records, err := installer.InstallFresh(ctx, []migrations.Migration{first, second}, "SELECT 42")
	if !errors.Is(err, migrations.ErrExecutionFailed) || len(records) != 0 {
		t.Fatal("partial fresh install escaped", err)
	}
	assertExpectations(t, mock)
}
