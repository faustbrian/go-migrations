//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v3"
	"github.com/faustbrian/go-migrations/v3/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type ledgerDiscoverySource struct{ step migrations.Migration }

func (s ledgerDiscoverySource) Load(ctx context.Context) ([]migrations.Migration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []migrations.Migration{s.step}, nil
}

// MIG-04 uses only an explicit disposable loopback cluster. PostgreSQL rejects
// DDL on the read-only pool, including the legacy Status preparation path.
func TestReadLedgerAgainstReadOnlyPostgreSQL(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	admin, readonly := isolatedMigrationDatabase(t, ctx)
	reader, err := postgres.New(readonly, postgres.WithOperationTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertReadOnlyDiscovery(t, ctx, admin, readonly, reader)
}

// Each caller owns a unique database on the explicitly authorized disposable
// cluster; no test can install schema into another test's ledger.
func isolatedMigrationDatabase(t *testing.T, ctx context.Context) (*sql.DB, *sql.DB) {
	t.Helper()
	portText, passwordFile := os.Getenv("MIGRATION_DISCOVERY_POSTGRES_PORT"), os.Getenv("MIGRATION_DISCOVERY_PASSWORD_FILE")
	var u url.URL
	if portText == "" && passwordFile == "" {
		// Standard native/CI runs use the existing container boundary. Explicit
		// local fixture inputs remain an all-or-nothing opt-in, never fallback
		// from an invalid or partially configured external connection.
		var credential [32]byte
		if _, err := rand.Read(credential[:]); err != nil {
			t.Fatal("cannot create disposable credential")
		}
		version := os.Getenv("POSTGRES_VERSION")
		if version == "" {
			version = "18"
		}
		container, err := tcpostgres.Run(ctx, "postgres:"+version+"-alpine",
			tcpostgres.WithDatabase("migrations_test"), tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword(hex.EncodeToString(credential[:])), tcpostgres.BasicWaitStrategies())
		clear(credential[:])
		if container != nil {
			testcontainers.CleanupContainer(t, container)
		}
		if err != nil {
			t.Fatal("cannot start disposable migration container")
		}
		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatal("disposable migration connection unavailable")
		}
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("disposable migration connection rejected")
		}
		u = *parsed
	} else {
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			t.Fatal("explicit disposable PostgreSQL port required")
		}
		password, err := os.ReadFile(passwordFile)
		if err != nil || len(password) < 32 || len(password) > 128 {
			t.Fatal("disposable PostgreSQL credential required")
		}
		u = url.URL{Scheme: "postgres", Host: net.JoinHostPort("127.0.0.1", portText), Path: "/migrations_test", User: url.UserPassword("postgres", strings.TrimSpace(string(password)))}
		clear(password)
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	root, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal("cannot open fixture owner")
	}
	t.Cleanup(func() { _ = root.Close() })
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("cannot name isolated database")
	}
	name := "discovery_" + hex.EncodeToString(nonce[:])
	if _, err := root.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal("cannot create isolated fixture database")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := root.ExecContext(cleanup, `DROP DATABASE "`+name+`"`); err != nil {
			t.Error("isolated fixture database cleanup failed")
		}
	})
	u.Path = "/" + name
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal("cannot open disposable fixture")
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Fatal("disposable fixture unavailable")
	}
	// Pass the startup setting directly: libpq-style URL parsing preserves
	// literal '+' characters instead of form-decoding option-string spaces.
	q.Set("default_transaction_read_only", "on")
	u.RawQuery = q.Encode()
	readonly, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal("cannot open read-only fixture")
	}
	t.Cleanup(func() { _ = readonly.Close() })
	return admin, readonly
}

func assertReadOnlyDiscovery(t *testing.T, ctx context.Context, admin, readonly *sql.DB, reader *postgres.Backend) {
	t.Helper()
	records, err := reader.ReadLedger(ctx)
	if err != nil || len(records) != 0 {
		t.Fatal("fresh read-only discovery attempted preparation", err)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, "SELECT to_regclass('public.migrations') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatal("discovery created a ledger")
	}
	step, err := migrations.NewMigration(1, "discovery_consumer", migrations.TransactionModeDefault, "CREATE TABLE public.discovery_consumer(id bigint PRIMARY KEY)", "")
	if err != nil {
		t.Fatal(err)
	}
	source := ledgerDiscoverySource{step}
	plan, err := migrations.PlanUp([]migrations.Migration{step}, records)
	if err != nil || len(plan.Steps()) != 1 {
		t.Fatal("empty authority could not plan native source", err)
	}
	legacy, err := migrations.NewRunner(source, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Status(ctx); !errors.Is(err, postgres.ErrDatabaseOperationFailed) {
		t.Fatal("fixture did not enforce read-only DDL denial")
	}
	writer, err := postgres.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := migrations.NewRunner(source, writer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Up(ctx); err != nil {
		t.Fatal("native migration install failed", err)
	}
	records, err = reader.ReadLedger(ctx)
	if err != nil || len(records) != 1 || records[0].Version() != 1 || records[0].Checksum() != step.Checksum() || records[0].Dirty() {
		t.Fatal("installed native identity changed", err)
	}
	// Waiting on a real exclusive DDL lock must honor the discovery budget and
	// release its borrowed transaction/connection after cancellation.
	lock, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("cannot establish fixture lock")
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(ctx, "LOCK TABLE public.migrations IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal("cannot hold fixture lock")
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = reader.ReadLedger(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked discovery ignored deadline", err)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal("cannot release fixture lock")
	}
	if _, err := admin.ExecContext(ctx, "UPDATE public.migrations SET dirty=true, finished_at=NULL WHERE version=1"); err != nil {
		t.Fatal("cannot establish dirty history")
	}
	records, err = reader.ReadLedger(ctx)
	if err != nil || len(records) != 1 || !records[0].Dirty() {
		t.Fatal("dirty history hidden", err)
	}
	if _, err := migrations.PlanUp([]migrations.Migration{step}, records); !errors.Is(err, migrations.ErrDirty) {
		t.Fatal("dirty history planned as healthy")
	}
	if _, err := admin.ExecContext(ctx, "DROP TABLE public.migrations; CREATE VIEW public.migrations AS SELECT 1 AS version"); err != nil {
		t.Fatal("cannot establish foreign relation fixture")
	}
	if records, err := reader.ReadLedger(ctx); !errors.Is(err, postgres.ErrLedgerConflict) || len(records) != 0 {
		t.Fatal("foreign relation admitted", err)
	}
	if err := readonly.PingContext(ctx); err != nil {
		t.Fatal("borrowed pool was closed")
	}
}
