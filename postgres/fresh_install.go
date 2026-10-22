package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	migrations "github.com/faustbrian/go-migrations/v4"
	gooseadapter "github.com/faustbrian/go-migrations/v4/internal/goose"
)

// ErrInstallOutcomeUnknown means fresh installation may have committed. Read
// the authoritative ledger under migration ownership before deciding whether
// installation succeeded; never blindly retry setup or infer rollback.
var ErrInstallOutcomeUnknown = errors.New("PostgreSQL fresh installation outcome unknown")

// InstallFresh installs a complete ordered history and optional package-owned
// preflight SQL in one transaction, retaining each original version/checksum.
// Ledger creation, preflight effects and every migration roll back together on
// execution failure. Existing nonempty history returns ErrLedgerConflict before
// preflight execution; this is not an upgrade, baseline or automatic retry API.
//
// Only reviewed transactional SQL is accepted: preflight and migration SQL must
// not contain transaction-control statements or nontransactional effects. Empty
// histories, invalid ordering, no-transaction steps and combined SQL exceeding
// MaxMigrationSourceBytes deny before database IO. Input is copied for this call;
// callers must not mutate the slice concurrently with entry.
//
// The stable native advisory lock covers discovery, execution and finalization.
// Work and lock acquisition are bounded by backend policy/context; independent
// cleanup uses a finite deadline. The database is borrowed and stays open.
// Error formatting redacts SQL/driver diagnostics, but traversal may expose
// underlying driver causes; do not log unwrapped diagnostics. A commit or
// post-commit unlock failure
// returns ErrInstallOutcomeUnknown and no success records. Nil/canceled contexts
// deny before IO. Concurrent calls serialize through native migration ownership.
// This API does not change Runner's individual-migration transaction semantics.
func (backend *Backend) InstallFresh(ctx context.Context, history []migrations.Migration, preflightSQL string) (records []migrations.Record, err error) {
	if backend == nil || backend.database == nil || ctx == nil {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounded, cancel := boundedOperationContext(ctx, backend.operationTimeout)
	defer cancel()
	if len(history) == 0 || len(history) > migrations.MaxMigrationFiles {
		return nil, migrations.ErrInvalidSource
	}
	if len(preflightSQL) > migrations.MaxMigrationSourceBytes || !utf8.ValidString(preflightSQL) || strings.IndexByte(preflightSQL, 0) >= 0 || (preflightSQL != "" && strings.TrimSpace(preflightSQL) == "") {
		return nil, migrations.ErrInvalidEncoding
	}
	history = slices.Clone(history)
	if _, err := migrations.PlanUp(history, nil); err != nil {
		return nil, err
	}
	total := len(preflightSQL)
	adapters := make([]*gooseadapter.Adapter, 0, len(history))
	for _, step := range history {
		if step.TransactionMode() != migrations.TransactionModeDefault {
			return nil, migrations.ErrInvalidTransactionMode
		}
		if len(step.UpSQL()) > migrations.MaxMigrationSourceBytes-total {
			return nil, migrations.ErrSourceLimit
		}
		total += len(step.UpSQL())
		if len(step.DownSQL()) > migrations.MaxMigrationSourceBytes-total {
			return nil, migrations.ErrSourceLimit
		}
		total += len(step.DownSQL())
		adapter, compileErr := gooseadapter.Compile(step)
		if compileErr != nil {
			return nil, compileErr
		}
		adapters = append(adapters, adapter)
	}
	owned, err := backend.Acquire(bounded)
	if err != nil {
		return nil, err
	}
	session := owned.(*session) // Acquire above creates only the native session.
	committed := false
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), statementTimeoutResetLimit)
		defer stop()
		if releaseErr := session.Release(cleanup); releaseErr != nil {
			records = nil
			err = errors.Join(err, releaseErr)
			if committed {
				err = errors.Join(err, ErrInstallOutcomeUnknown)
			}
		}
	}()
	tx, err := session.connection.BeginTx(bounded, nil)
	if err != nil {
		return nil, databaseContextFailure(bounded, "begin fresh installation", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			session.tainted = true
			records = nil
			err = errors.Join(err, databaseFailure("rollback fresh installation", rollbackErr))
		}
	}()
	if err := setLocalStatementTimeout(bounded, tx, backend.statementTimeout); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(bounded, ledgerSQL(createLedgerSQL, session.namespace)); err != nil {
		return nil, databaseContextFailure(bounded, "prepare fresh installation ledger", err)
	}
	rows, err := tx.QueryContext(bounded, ledgerSQL(ledgerRecordsSQL, session.namespace))
	if err != nil {
		return nil, databaseContextFailure(bounded, "read fresh installation ledger", err)
	}
	existing, readErr := decodeLedgerRows(bounded, rows)
	closeErr := rows.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, databaseFailure("close fresh installation ledger", closeErr))
	}
	if len(existing) != 0 {
		return nil, ErrLedgerConflict
	}
	if preflightSQL != "" {
		if _, err := tx.ExecContext(bounded, preflightSQL); err != nil {
			return nil, databaseContextFailure(bounded, "execute fresh installation preflight", err)
		}
	}
	records = make([]migrations.Record, 0, len(history))
	for index, step := range history {
		started := time.Now().UTC()
		if err := insertDirty(bounded, tx, step, started, session.namespace); err != nil {
			return nil, err
		}
		if err := adapters[index].ApplyTx(bounded, tx); err != nil {
			return nil, err
		}
		finished := time.Now().UTC()
		duration := finished.Sub(started)
		if err := markClean(bounded, tx, step, finished, duration, session.namespace); err != nil {
			return nil, err
		}
		record, err := migrations.NewRecord(migrations.RecordKindMigration, step.Version(), step.Name(), step.Checksum(), finished, duration, false)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.Join(ErrInstallOutcomeUnknown, databaseContextFailure(bounded, "commit fresh installation", err))
	}
	committed = true
	return records, nil
}
