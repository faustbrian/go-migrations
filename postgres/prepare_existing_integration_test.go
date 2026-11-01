//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	migrations "github.com/faustbrian/go-migrations/v4"
	"github.com/faustbrian/go-migrations/v4/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestRunnerUsesExistingLedgerWithoutSchemaCreate(t *testing.T) {
	for _, namespace := range []string{"", "restricted"} {
		t.Run("namespace="+namespace, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			admin, _ := isolatedMigrationDatabase(t, ctx)
			database, role := existingLedgerOwner(t, ctx, admin)
			backend, err := postgres.New(database)
			if err != nil {
				t.Fatal(err)
			}
			ledger := "public.migrations"
			if namespace != "" {
				backend, err = backend.ForNamespace(namespace)
				if err != nil {
					t.Fatal(err)
				}
				ledger = "public.golib_migrations_restricted"
			}
			files := fstest.MapFS{
				"migrations/000001_first.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nINSERT INTO application.prepare_rows VALUES(1);\n-- +migrations Down\nDELETE FROM application.prepare_rows WHERE id=1;\n")},
			}
			runner := func(source fstest.MapFS) *migrations.Runner {
				t.Helper()
				loaded, loadErr := migrations.NewFSSource(integrationSourceFileSystem{files: source}, "migrations")
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				value, runnerErr := migrations.NewRunner(loaded, backend)
				if runnerErr != nil {
					t.Fatal(runnerErr)
				}
				return value
			}
			bootstrap, err := backend.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			prepareErr := bootstrap.Prepare(ctx)
			releaseErr := bootstrap.Release(ctx)
			if prepareErr != nil || releaseErr != nil {
				t.Fatal("initialize empty selected ledger", prepareErr, releaseErr)
			}
			if _, err := admin.ExecContext(ctx, "REVOKE CREATE ON SCHEMA public FROM "+role); err != nil {
				t.Fatal(err)
			}
			assertNoPublicCreate(t, ctx, database)
			if _, err := runner(files).Status(ctx); err != nil {
				t.Fatal("read empty selected ledger without schema CREATE", err)
			}
			if result, err := runner(files).Up(ctx); err != nil || len(result.Records()) != 1 {
				t.Fatal("initialize selected ledger history without schema CREATE", err)
			}
			snapshot := func(condition string) string {
				t.Helper()
				var value string
				if err := admin.QueryRowContext(ctx, "SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY version),'[]'::jsonb)::text FROM "+ledger+" m "+condition).Scan(&value); err != nil {
					t.Fatal("snapshot selected ledger", err)
				}
				return value
			}
			previous := snapshot("")
			if _, err := admin.ExecContext(ctx, "CREATE SCHEMA decoy; CREATE TABLE decoy.migrations AS SELECT * FROM "+ledger+" WITH NO DATA; GRANT USAGE ON SCHEMA decoy TO "+role+"; GRANT SELECT ON decoy.migrations TO "+role+"; REVOKE CREATE ON SCHEMA public FROM "+role); err != nil {
				t.Fatal("establish restricted existing owner", err)
			}
			if _, err := database.ExecContext(ctx, "SET search_path=decoy,public"); err != nil {
				t.Fatal("set hostile search path", err)
			}
			assertNoPublicCreate(t, ctx, database)
			var shadowed bool
			if err := database.QueryRowContext(ctx, "SELECT to_regclass('migrations')='decoy.migrations'::regclass").Scan(&shadowed); err != nil || !shadowed {
				t.Fatal("decoy did not shadow unqualified ledger", err)
			}
			session, err := backend.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			prepareErr = session.Prepare(ctx)
			var records []migrations.Record
			if prepareErr == nil {
				records, prepareErr = session.Records(ctx)
			}
			releaseErr = session.Release(ctx)
			if prepareErr != nil || releaseErr != nil || len(records) != 1 {
				t.Fatal("prepare existing selected ledger without public CREATE", prepareErr, releaseErr)
			}
			files["migrations/000002_second.sql"] = &fstest.MapFile{Data: []byte("-- +migrations Up\nINSERT INTO application.prepare_rows VALUES(2);\n-- +migrations Down\nDELETE FROM application.prepare_rows WHERE id=2;\n")}
			if plan, err := runner(files).Plan(ctx); err != nil || len(plan.Steps()) != 1 {
				t.Fatal("plan existing history", err)
			}
			if _, err := runner(files).Status(ctx); err != nil || snapshot("") != previous {
				t.Fatal("status changed existing history", err)
			}
			if result, err := runner(files).Up(ctx); err != nil || len(result.Records()) != 1 || snapshot("WHERE version=1") != previous {
				t.Fatal("append under restricted ledger owner", err)
			}
			after := snapshot("")
			missing, err := backend.ForNamespace("absent")
			if err != nil {
				t.Fatal(err)
			}
			missingSession, err := missing.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			prepareErr = missingSession.Prepare(ctx)
			releaseErr = missingSession.Release(ctx)
			var denied *pgconn.PgError
			if !errors.As(prepareErr, &denied) || denied.Code != "42501" || releaseErr != nil || snapshot("") != after {
				t.Fatal("missing namespace bypassed schema creation authority", prepareErr, releaseErr)
			}
			var absent bool
			if err := admin.QueryRowContext(ctx, "SELECT to_regclass('public.golib_migrations_absent') IS NULL").Scan(&absent); err != nil || !absent {
				t.Fatal("missing namespace was created or adopted", err)
			}
			if _, err := admin.ExecContext(ctx, "CREATE VIEW public.golib_migrations_foreign AS SELECT * FROM "+ledger); err != nil {
				t.Fatal(err)
			}
			foreign, err := backend.ForNamespace("foreign")
			if err != nil {
				t.Fatal(err)
			}
			foreignSession, err := foreign.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			prepareErr = foreignSession.Prepare(ctx)
			releaseErr = foreignSession.Release(ctx)
			if !errors.Is(prepareErr, postgres.ErrLedgerConflict) || releaseErr != nil || snapshot("") != after {
				t.Fatal("foreign view was accepted as a selected ledger", prepareErr, releaseErr)
			}
			missingSource := fstest.MapFS{"migrations/000001_first.sql": files["migrations/000001_first.sql"]}
			if _, err := runner(missingSource).Up(ctx); !errors.Is(err, migrations.ErrDeletedMigration) || snapshot("") != after {
				t.Fatal("missing source bypassed history validation", err)
			}
			forbidden := fstest.MapFS{
				"migrations/000001_first.sql":  files["migrations/000001_first.sql"],
				"migrations/000002_second.sql": files["migrations/000002_second.sql"],
				"migrations/000003_denied.sql": &fstest.MapFile{Data: []byte("-- +migrations Up\nCREATE TABLE public.forbidden(id bigint);\n")},
			}
			if _, err := runner(forbidden).Up(ctx); !errors.Is(err, migrations.ErrExecutionFailed) || snapshot("") != after {
				t.Fatal("existing ledger bypassed migration SQL privileges or mutated history", err)
			}
			if err := admin.QueryRowContext(ctx, "SELECT to_regclass('public.forbidden') IS NULL").Scan(&absent); err != nil || !absent {
				t.Fatal("forbidden migration SQL took effect", err)
			}
			if result, err := runner(files).Up(ctx); err != nil || len(result.Records()) != 0 || snapshot("") != after {
				t.Fatal("retry changed complete history", err)
			}
			var rows int
			if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM application.prepare_rows").Scan(&rows); err != nil || rows != 2 {
				t.Fatal("appended SQL missing", err)
			}
			drift := fstest.MapFS{
				"migrations/000001_first.sql":  &fstest.MapFile{Data: []byte("-- +migrations Up\nSELECT 99;\n")},
				"migrations/000002_second.sql": files["migrations/000002_second.sql"],
			}
			if _, err := runner(drift).Up(ctx); !errors.Is(err, migrations.ErrChecksumMismatch) || snapshot("") != after {
				t.Fatal("checksum drift bypassed history validation", err)
			}
			var finished time.Time
			if err := admin.QueryRowContext(ctx, "SELECT finished_at FROM "+ledger+" WHERE version=2").Scan(&finished); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(ctx, "UPDATE "+ledger+" SET dirty=true,finished_at=NULL WHERE version=2"); err != nil {
				t.Fatal(err)
			}
			dirty := snapshot("")
			if _, err := runner(files).Up(ctx); !errors.Is(err, migrations.ErrDirty) || snapshot("") != dirty {
				t.Fatal("dirty history was accepted or rewritten", err)
			}
			if _, err := admin.ExecContext(ctx, "UPDATE "+ledger+" SET dirty=false,finished_at=$1 WHERE version=2", finished); err != nil {
				t.Fatal(err)
			}
			if result, err := runner(files).Down(ctx, 1); err != nil || len(result.Records()) != 1 || snapshot("") != previous {
				t.Fatal("rollback changed retained earlier history", err)
			}
			assertNoPublicCreate(t, ctx, database)
			if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM decoy.migrations").Scan(&rows); err != nil || rows != 0 {
				t.Fatal("modified search-path decoy", err)
			}
		})
	}
}

func existingLedgerOwner(t *testing.T, ctx context.Context, admin *sql.DB) (*sql.DB, string) {
	t.Helper()
	config := commandFixturePGXConfig(t, ctx, admin)
	role := pgx.Identifier{"prepare_" + config.Database}.Sanitize()
	var credential [32]byte
	if _, err := rand.Read(credential[:]); err != nil {
		t.Fatal("generate disposable role credential")
	}
	password := hex.EncodeToString(credential[:])
	clear(credential[:])
	// PostgreSQL 14's schema defaults grant CREATE to PUBLIC (manual 5.9.4).
	// Normalize only this disposable database before testing the owner's revoke.
	if _, err := admin.ExecContext(ctx, "REVOKE CREATE ON SCHEMA public FROM PUBLIC; CREATE ROLE "+role+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD '"+password+"'; GRANT USAGE,CREATE ON SCHEMA public TO "+role+"; CREATE SCHEMA application AUTHORIZATION "+role); err != nil {
		t.Fatal("create disposable schema identity", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanup, "DROP OWNED BY "+role+" CASCADE; DROP ROLE "+role); err != nil {
			t.Error("remove disposable schema identity", err)
		}
	})
	config.User = "prepare_" + config.Database
	config.Password = password
	database := stdlib.OpenDB(*config)
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.ExecContext(ctx, "CREATE TABLE application.prepare_rows(id bigint PRIMARY KEY)"); err != nil {
		t.Fatal("create owned application table", err)
	}
	return database, role
}

func assertNoPublicCreate(t *testing.T, ctx context.Context, database *sql.DB) {
	t.Helper()
	var canCreate bool
	if err := database.QueryRowContext(ctx, "SELECT has_schema_privilege(current_user,'public','CREATE')").Scan(&canCreate); err != nil || canCreate {
		t.Fatal("restricted owner retains public CREATE", err)
	}
}
