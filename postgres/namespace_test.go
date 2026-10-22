package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/faustbrian/go-migrations/v4/postgres"
)

type namespaceSelector interface {
	ForNamespace(string) (*postgres.Backend, error)
}

func TestNamespaceSelectionIsExplicitAndDoesNotAdoptDefaultHistory(t *testing.T) {
	db, mock := newMockDatabase(t)
	backend, err := postgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	selector, ok := any(backend).(namespaceSelector)
	if !ok {
		t.Fatal("native backend cannot preserve independent owner histories")
	}
	scoped, err := selector.ForNamespace("outbox")
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*relkind.*relname='golib_migrations_outbox'").WillReturnRows(sqlmock.NewRows([]string{"relkind"}))
	mock.ExpectRollback()
	if records, err := scoped.ReadLedger(context.Background()); err != nil || len(records) != 0 {
		t.Fatal("missing owner history adopted another ledger", err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*relkind.*relname='migrations'").WillReturnRows(sqlmock.NewRows([]string{"relkind"}))
	mock.ExpectRollback()
	if records, err := backend.ReadLedger(context.Background()); err != nil || len(records) != 0 {
		t.Fatal("selection mutated original backend", err)
	}
	assertExpectations(t, mock)
}

func TestNamespaceSelectionRejectsAliasesBeforeIO(t *testing.T) {
	db, mock := newMockDatabase(t)
	backend, _ := postgres.New(db)
	selector, ok := any(backend).(namespaceSelector)
	if !ok {
		t.Fatal("native backend cannot select independent owner history")
	}
	for _, namespace := range []string{"", "Upper", "a.b", "a\"", "a--b", "a b", "a\x00", "\xff", "_owner", "owner_", "owner__two", strings.Repeat("a", 47)} {
		if selected, err := selector.ForNamespace(namespace); !errors.Is(err, postgres.ErrInvalidConfig) || selected != nil {
			t.Fatal("invalid namespace admitted")
		}
	}
	for _, namespace := range []string{"a", "a0", "owner_two", strings.Repeat("a", 46)} {
		if selected, err := selector.ForNamespace(namespace); err != nil || selected == nil {
			t.Fatal("valid namespace rejected", err)
		}
	}
	assertExpectations(t, mock)
}
