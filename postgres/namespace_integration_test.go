//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	migrations "github.com/faustbrian/go-migrations/v4"
	"github.com/faustbrian/go-migrations/v4/postgres"
)

type namespaceHistory []migrations.Migration

func (s namespaceHistory) Load(ctx context.Context) ([]migrations.Migration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]migrations.Migration(nil), s...), nil
}

func TestNamespaceIndependentHistoriesPostgreSQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	db, readonly := isolatedMigrationDatabase(t, ctx)
	base, _ := postgres.New(db, postgres.WithLockTimeout(100*time.Millisecond))
	selector, ok := any(base).(namespaceSelector)
	if !ok {
		t.Fatal("native backend cannot preserve independent owner histories")
	}
	owner := func(namespace string) *postgres.Backend {
		t.Helper()
		selected, err := selector.ForNamespace(namespace)
		if err != nil {
			t.Fatal(err)
		}
		return selected
	}
	step := func(name, up, down string, mode migrations.TransactionMode) migrations.Migration {
		t.Helper()
		value, err := migrations.NewMigration(1, name, mode, up, down)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	assertHistory := func(backend *postgres.Backend, history []migrations.Migration, dirty bool) {
		t.Helper()
		records, err := backend.ReadLedger(ctx)
		if err != nil || len(records) != len(history) {
			t.Fatal("owner history missing or crossed", err)
		}
		for i, record := range records {
			if record.Version() != history[i].Version() || record.Name() != history[i].Name() || record.Checksum() != history[i].Checksum() || record.Dirty() != dirty {
				t.Fatal("owner identity or dirty state changed")
			}
		}
	}
	defaultStep := step("default_owner", "CREATE TABLE public.default_owner(id bigint)", "", migrations.TransactionModeDefault)
	if _, err := base.InstallFresh(ctx, []migrations.Migration{defaultStep}, ""); err != nil {
		t.Fatal(err)
	}
	first := step("first_owner", "CREATE TABLE public.first_owner(id bigint)", "", migrations.TransactionModeDefault)
	second := step("second_owner", "CREATE TABLE public.second_owner(id bigint)", "", migrations.TransactionModeDefault)
	a, b := owner("outbox"), owner("lifecycle")
	if _, err := a.InstallFresh(ctx, []migrations.Migration{first}, ""); err != nil {
		t.Fatal(err)
	}
	readBase, _ := postgres.New(readonly)
	readB, err := readBase.ForNamespace("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	assertHistory(readB, nil, false)
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.golib_migrations_lifecycle') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatal("read-only owner discovery prepared schema")
	}
	if _, err := b.InstallFresh(ctx, []migrations.Migration{second}, ""); err != nil {
		t.Fatal(err)
	}
	assertHistory(a, []migrations.Migration{first}, false)
	assertHistory(readB, []migrations.Migration{second}, false)
	assertHistory(base, []migrations.Migration{defaultStep}, false)
	if _, err := b.InstallFresh(ctx, []migrations.Migration{second}, "SELECT 1/0"); !errors.Is(err, postgres.ErrLedgerConflict) {
		t.Fatal("selected history did not deny before preflight", err)
	}

	// A failed third owner must not retain its preflight, ledger or first step.
	failed := owner("failed")
	good := step("failed_first", "CREATE TABLE public.failed_first(id bigint)", "", migrations.TransactionModeDefault)
	bad, _ := migrations.NewMigration(2, "failed_second", migrations.TransactionModeDefault, "SELECT 1/0", "")
	if _, err := failed.InstallFresh(ctx, []migrations.Migration{good, bad}, "CREATE TABLE public.failed_setup(id bigint)"); !errors.Is(err, migrations.ErrExecutionFailed) {
		t.Fatal("failed owner install escaped", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.failed_first') IS NOT NULL OR to_regclass('public.failed_setup') IS NOT NULL OR to_regclass('public.golib_migrations_failed') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatal("failed selected install retained effects")
	}
	assertHistory(base, []migrations.Migration{defaultStep}, false)
	assertHistory(a, []migrations.Migration{first}, false)

	// Both ordinary execution modes prepare, persist and remove only their owner.
	for _, mode := range []migrations.TransactionMode{migrations.TransactionModeDefault, migrations.TransactionModeNone} {
		name := "transactional"
		if mode == migrations.TransactionModeNone {
			name = "unwrapped"
		}
		selected := owner(name)
		migration := step(name, "CREATE TABLE public."+name+"(id bigint)", "DROP TABLE public."+name, mode)
		runner, err := migrations.NewRunner(namespaceHistory{migration}, selected)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Up(ctx); err != nil {
			t.Fatal("selected Up failed", err)
		}
		assertHistory(selected, []migrations.Migration{migration}, false)
		if _, err := runner.Down(ctx, 1); err != nil {
			t.Fatal("selected Down failed", err)
		}
		assertHistory(selected, nil, false)
		if err := db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&exists); err != nil || exists {
			t.Fatal("selected rollback omitted schema effect")
		}
		assertHistory(base, []migrations.Migration{defaultStep}, false)
	}

	// Identical version/checksum on two dirty owners cannot conceal wrong-target
	// recovery behind checksum predicates.
	recoveryStep := step("recovery", "SELECT 1", "SELECT 1", migrations.TransactionModeNone)
	ra, rb := owner("recover_a"), owner("recover_b")
	for _, selected := range []*postgres.Backend{ra, rb} {
		runner, _ := migrations.NewRunner(namespaceHistory{recoveryStep}, selected)
		if _, err := runner.Up(ctx); err != nil {
			t.Fatal(err)
		}
	}
	seedDirty := func(table string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "UPDATE public."+table+" SET dirty=true, finished_at=NULL"); err != nil {
			t.Fatal("cannot seed owner recovery fixture")
		}
	}
	seedDirty("golib_migrations_recover_a")
	seedDirty("golib_migrations_recover_b")
	runner, _ := migrations.NewRunner(namespaceHistory{recoveryStep}, ra)
	decision, _ := migrations.NewRecovery(1, recoveryStep.Checksum(), migrations.RecoveryMarkApplied)
	if _, err := runner.Recover(ctx, decision); err != nil {
		t.Fatal("selected recovery apply failed", err)
	}
	assertHistory(ra, []migrations.Migration{recoveryStep}, false)
	assertHistory(rb, []migrations.Migration{recoveryStep}, true)
	seedDirty("golib_migrations_recover_a")
	decision, _ = migrations.NewRecovery(1, recoveryStep.Checksum(), migrations.RecoveryMarkRolledBack)
	if _, err := runner.Recover(ctx, decision); err != nil {
		t.Fatal("selected recovery removal failed", err)
	}
	assertHistory(ra, nil, false)
	assertHistory(rb, []migrations.Migration{recoveryStep}, true)
	if _, err := runner.Recover(ctx, decision); !errors.Is(err, migrations.ErrNoDirtyMigration) {
		t.Fatal("missing selected recovery adopted neighbor", err)
	}
	readRecovery, err := readBase.ForNamespace("recover_b")
	if err != nil {
		t.Fatal(err)
	}
	assertHistory(readRecovery, []migrations.Migration{recoveryStep}, true)

	// A failed unwrapped statement retains dirty state only in its own ledger.
	dirtyOwner := owner("dirty_failure")
	dirtyStep := step("dirty_failure", "SELECT 1/0", "", migrations.TransactionModeNone)
	dirtyRunner, _ := migrations.NewRunner(namespaceHistory{dirtyStep}, dirtyOwner)
	if _, err := dirtyRunner.Up(ctx); !errors.Is(err, migrations.ErrExecutionFailed) {
		t.Fatal("unwrapped selected failure escaped", err)
	}
	assertHistory(dirtyOwner, []migrations.Migration{dirtyStep}, true)
	assertHistory(base, []migrations.Migration{defaultStep}, false)

	foreign := owner("foreign")
	if _, err := db.ExecContext(ctx, "CREATE VIEW public.golib_migrations_foreign AS SELECT 1 AS version"); err != nil {
		t.Fatal("cannot establish selected foreign relation")
	}
	if records, err := foreign.ReadLedger(ctx); !errors.Is(err, postgres.ErrLedgerConflict) || len(records) != 0 {
		t.Fatal("selected foreign relation admitted", err)
	}

	// Inspection excludes only the selected ledger, including its RLS policies;
	// other owner ledgers and ordinary database schema remain fingerprint inputs.
	if _, err := db.ExecContext(ctx, "ALTER TABLE public.golib_migrations_outbox ENABLE ROW LEVEL SECURITY; CREATE POLICY selected_policy ON public.golib_migrations_outbox USING (true)"); err != nil {
		t.Fatal("cannot establish ledger policy")
	}
	objects, err := a.InspectObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defaultVisible := false
	for _, object := range objects {
		if strings.Contains(object.Identity, "public.golib_migrations_outbox") {
			t.Fatal("selected ledger leaked into schema snapshot")
		}
		if object.Identity == "relation:public.migrations" {
			defaultVisible = true
		}
	}
	if !defaultVisible {
		t.Fatal("inspection hid nonselected default ledger")
	}
	baselineOwner := owner("baseline")
	fingerprint, err := baselineOwner.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	baseline, _ := migrations.NewBaseline(1, "reviewed_schema", fingerprint)
	baselineRunner, _ := migrations.NewRunner(namespaceHistory{}, baselineOwner)
	if _, err := baselineRunner.Baseline(ctx, baseline); err != nil {
		t.Fatal("selected baseline failed", err)
	}
	baselineRecords, err := baselineOwner.ReadLedger(ctx)
	if err != nil || len(baselineRecords) != 1 || baselineRecords[0].Kind() != migrations.RecordKindBaseline || baselineRecords[0].Checksum() != fingerprint {
		t.Fatal("baseline persisted outside owner", err)
	}
	assertHistory(base, []migrations.Migration{defaultStep}, false)

	// Distinct maximum-length IDs must not alias through PostgreSQL truncation.
	maximumStep := step("maximum", "SELECT 1", "", migrations.TransactionModeDefault)
	for _, suffix := range []string{"b", "c"} {
		maximum := owner(strings.Repeat("a", 45) + suffix)
		if _, err := maximum.InstallFresh(ctx, []migrations.Migration{maximumStep}, ""); err != nil {
			t.Fatal("maximum owner aliased or failed", err)
		}
		assertHistory(maximum, []migrations.Migration{maximumStep}, false)
	}
	// Independent instances still share native migration ownership.
	held, err := a.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release(context.Background())
	short, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	_, err = b.Acquire(short)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("owner namespaces bypassed shared lock", err)
	}
	if err := held.Release(ctx); err != nil {
		t.Fatal(err)
	}
	reacquired, err := b.Acquire(ctx)
	if err != nil {
		t.Fatal("namespace release retained shared ownership", err)
	}
	if err := reacquired.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("namespace owner closed borrowed pool")
	}
}
