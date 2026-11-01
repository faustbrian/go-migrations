package postgres

import (
	"context"
	"database/sql"
	"errors"

	migrations "github.com/faustbrian/go-migrations/v4"
)

const ledgerRecordsSQL = "SELECT kind, version, name, checksum, started_at, finished_at, execution_time_ms, dirty FROM public.migrations ORDER BY version ASC"

// ReadLedger discovers the existing selected ledger history without
// preparing schema or acquiring the migration advisory lock. A missing ledger
// yields empty history; a non-table relation at that name returns
// ErrLedgerConflict. It never discovers or adopts another ledger name.
//
// Discovery uses one bounded repeatable-read, read-only transaction and the
// same row validation and resource limits as Session.Records. Returned records
// are caller-owned; dirty records remain dirty. The borrowed database stays
// open. Error formatting is redacted; errors.Is preserves classification and
// error traversal may expose driver causes. Do not log unwrapped diagnostics.
// Concurrent calls are safe. Nil/canceled contexts deny before database IO.
//
// The result is a snapshot, not migration ownership or permission to apply.
// Compose BuildStatus or PlanUp with the exact source to inspect compatibility;
// execution must rediscover and validate history under acquired ownership.
// Runner.Status/Plan retain their existing locked preparation behavior.
func (backend *Backend) ReadLedger(ctx context.Context) (records []migrations.Record, err error) {
	if backend == nil || backend.database == nil || ctx == nil {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounded, cancel := boundedOperationContext(ctx, backend.operationTimeout)
	defer cancel()
	return readOwnedLedger(bounded, backend.database, backend.namespace, nil)
}

type ledgerBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func readOwnedLedger(bounded context.Context, database ledgerBeginner, namespace string, rollbackFailed func()) (records []migrations.Record, err error) {
	tx, err := database.BeginTx(bounded, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, databaseContextFailure(bounded, "begin ledger discovery", err)
	}
	defer func() {
		if cleanupErr := tx.Rollback(); cleanupErr != nil && !errors.Is(cleanupErr, sql.ErrTxDone) {
			if rollbackFailed != nil {
				rollbackFailed()
			}
			records = nil
			err = errors.Join(err, databaseFailure("release ledger discovery", cleanupErr))
		}
	}()
	var kind string
	err = tx.QueryRowContext(bounded, `SELECT c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='`+ledgerName(namespace)+`'`).Scan(&kind)
	// A joined driver failure is not an empty result, even if one cause is
	// ErrNoRows. Keep those failures visible instead of reporting empty history.
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, databaseContextFailure(bounded, "discover migration ledger", err)
	}
	if kind != "r" && kind != "p" {
		return nil, ErrLedgerConflict
	}
	rows, err := tx.QueryContext(bounded, ledgerSQL(ledgerRecordsSQL, namespace))
	if err != nil {
		return nil, databaseContextFailure(bounded, "query migration ledger", err)
	}
	defer func() { _ = rows.Close() }()
	return decodeLedgerRows(bounded, rows)
}
