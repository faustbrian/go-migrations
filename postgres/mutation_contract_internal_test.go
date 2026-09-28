package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	migrations "github.com/faustbrian/go-migrations/v2"
)

func TestLedgerAcceptsCompleteBoundedHistory(t *testing.T) {
	t.Parallel()

	owned, mock := faultSession(t, 0)
	rows := sqlmock.NewRows([]string{
		"kind", "version", "name", "checksum", "started_at", "finished_at", "execution_time_ms", "dirty",
	})
	checksum := migrations.ChecksumData([]byte("reviewed schema")).String()
	appliedAt := time.Unix(1, 0).UTC()
	wantRecords := migrations.MaxMigrationFiles + 1
	rows.AddRow("baseline", 1, "baseline", checksum, appliedAt, appliedAt, 0, false)
	for version := 2; version <= wantRecords; version++ {
		rows.AddRow("migration", version, "migration", checksum, appliedAt, appliedAt, 0, false)
	}
	mock.ExpectQuery("SELECT (.+) FROM public.go_schema_migrations").WillReturnRows(rows)
	records, err := owned.Records(context.Background())
	if err != nil || len(records) != wantRecords {
		t.Fatalf("Records() count = %d, error = %v, want complete bounded history", len(records), err)
	}
	if records[0].Kind() != migrations.RecordKindBaseline || records[len(records)-1].Version() != migrations.Version(wantRecords) {
		t.Fatalf("Records() boundary identities = %v, %v", records[0], records[len(records)-1])
	}
	assertFaultExpectations(t, mock)
}

func TestDatabaseFailureQuotedDiagnosticIsStableAndRedacted(t *testing.T) {
	t.Parallel()

	cause := structuredDatabaseDiagnostic{Password: "private"}
	err := databaseFailure("inspect migration ledger", cause)
	want := fmt.Sprintf("%q", "inspect migration ledger: "+ErrDatabaseOperationFailed.Error())
	if got := fmt.Sprintf("%q", err); got != want {
		t.Fatalf("quoted database failure = %s, want %s", got, want)
	}
	if strings.Contains(fmt.Sprintf("%q", err), cause.Password) {
		t.Fatal("quoted database failure disclosed driver data")
	}
}

func TestTransactionalApplyRejectsFailedLocalTimeoutBeforeMigration(t *testing.T) {
	t.Parallel()

	owned, mock := faultSession(t, 250*time.Millisecond)
	migration := faultMigration(t, migrations.TransactionModeDefault)
	cause := structuredDatabaseDiagnostic{Password: "private"}
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnError(cause)
	mock.ExpectRollback()
	_, err := owned.Apply(context.Background(), migration)
	if !errors.Is(err, ErrDatabaseOperationFailed) || !errors.Is(err, cause) {
		t.Fatalf("Apply() timeout setup error = %v, want classified driver failure", err)
	}
	if strings.Contains(err.Error(), cause.Password) {
		t.Fatal("Apply() disclosed timeout setup driver data")
	}
	assertFaultExpectations(t, mock)
}

func TestNilOperationGateRejectsSessionBeforeDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, finish, err := (&session{operationTimeout: time.Second}).beginOperation(ctx)
	if !errors.Is(err, ErrSessionReleased) || finish != nil {
		t.Fatalf("beginOperation(nil gate) = finish %v, error %v; want released session", finish != nil, err)
	}
}

func TestReleaseCanceledWhileOperationOwnsGatePreservesSession(t *testing.T) {
	t.Parallel()

	owned, mock := faultSession(t, 0)
	release, err := owned.gate.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire operation gate: %v", err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := owned.Release(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Release(canceled) error = %v, want cancellation", err)
	}
	if owned.released || owned.connection == nil {
		t.Fatal("canceled Release changed connection ownership")
	}
	assertFaultExpectations(t, mock)
}

func TestFingerprintAcceptsExactObjectCountAndRejectsNext(t *testing.T) {
	t.Parallel()

	objects := make([]SchemaObject, MaxSchemaObjects)
	for index := range objects {
		objects[index] = SchemaObject{Identity: fmt.Sprintf("table:public.object_%05d", index), Definition: "CREATE TABLE object ();"}
	}
	if _, err := Fingerprint(objects); err != nil {
		t.Fatalf("Fingerprint(exact object count) error = %v", err)
	}
	objects = append(objects, SchemaObject{Identity: "table:public.extra", Definition: "CREATE TABLE extra ();"})
	if _, err := Fingerprint(objects); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("Fingerprint(next object) error = %v, want ErrResourceLimit", err)
	}
}

func TestSchemaObjectFieldBoundsAcceptExactAndRejectNextByte(t *testing.T) {
	t.Parallel()

	for _, field := range []struct {
		name  string
		limit int
		build func(string) SchemaObject
	}{
		{name: "identity", limit: MaxSchemaObjectIdentityBytes, build: func(value string) SchemaObject {
			return SchemaObject{Identity: value, Definition: "definition"}
		}},
		{name: "definition", limit: MaxSchemaObjectDefinitionBytes, build: func(value string) SchemaObject {
			return SchemaObject{Identity: "table:public.object", Definition: value}
		}},
	} {
		t.Run(field.name, func(t *testing.T) {
			value := strings.Repeat("x", field.limit)
			if _, err := Fingerprint([]SchemaObject{field.build(value)}); err != nil {
				t.Fatalf("Fingerprint(exact %s bytes) error = %v", field.name, err)
			}
			if _, err := Fingerprint([]SchemaObject{field.build(value + "x")}); !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("Fingerprint(next %s byte) error = %v, want ErrResourceLimit", field.name, err)
			}
		})
	}
}

func TestCatalogQueryFailureReturnsRedactedErrorWithoutRows(t *testing.T) {
	t.Parallel()

	database, mock := faultDatabase(t)
	backend, err := New(database)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cause := structuredDatabaseDiagnostic{Password: "private"}
	mock.ExpectQuery("SELECT object_identity, definition FROM schema_objects").WillReturnError(cause)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	objects, err := backend.InspectObjects(ctx)
	if objects != nil || !errors.Is(err, ErrDatabaseOperationFailed) || !errors.Is(err, cause) {
		t.Fatalf("InspectObjects(query failure) = %v, %v, want no objects and classified error", objects, err)
	}
	if strings.Contains(err.Error(), cause.Password) {
		t.Fatal("InspectObjects() disclosed catalog driver data")
	}
	assertFaultExpectations(t, mock)
}
