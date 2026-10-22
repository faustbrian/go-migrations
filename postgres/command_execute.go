package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	migrations "github.com/faustbrian/go-migrations/v4"
	gooseadapter "github.com/faustbrian/go-migrations/v4/internal/goose"
)

const commandTableMarker = "golib/migrations/command/v1"
const createCommandTableSQL = `CREATE TABLE public.golib_migration_commands (
 command_id text PRIMARY KEY CHECK(octet_length(command_id) BETWEEN 16 AND 128),
 request_digest bytea NOT NULL CHECK(octet_length(request_digest)=32),
 idempotency_digest bytea NOT NULL UNIQUE CHECK(octet_length(idempotency_digest)=32),
 job bytea NOT NULL CHECK(octet_length(job) BETWEEN 1 AND 16777216),
 receipts bytea NOT NULL CHECK(octet_length(receipts) BETWEEN 1 AND 16777216),
 completed integer NOT NULL CHECK(completed BETWEEN 0 AND 4096)
); COMMENT ON TABLE public.golib_migration_commands IS 'golib/migrations/command/v1'`

func commandRelation(ctx context.Context, conn *sql.Conn) (bool, error) {
	var kind string
	var marker sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT c.relkind::text,pg_catalog.obj_description(c.oid,'pg_class') FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='golib_migration_commands'`).Scan(&kind, &marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseContextFailure(ctx, "discover migration commands", err)
	}
	if kind != "r" || !marker.Valid || marker.String != commandTableMarker {
		return false, ErrLedgerConflict
	}
	return true, nil
}

func readNativeCommand(ctx context.Context, conn *sql.Conn, id string) (commandStored, bool, error) {
	exists, err := commandRelation(ctx, conn)
	if err != nil || !exists {
		return commandStored{}, false, err
	}
	var job, receipts, digest, keyDigest []byte
	var completed int
	err = conn.QueryRowContext(ctx, `SELECT CASE WHEN octet_length(request_digest)=32 THEN request_digest END,
 CASE WHEN octet_length(idempotency_digest)=32 THEN idempotency_digest END,
 CASE WHEN octet_length(job) BETWEEN 1 AND 16777216 THEN job END,
 CASE WHEN octet_length(receipts) BETWEEN 1 AND 16777216 THEN receipts END,completed
 FROM public.golib_migration_commands WHERE command_id=$1`, id).Scan(&digest, &keyDigest, &job, &receipts, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return commandStored{}, false, nil
	}
	if err != nil {
		return commandStored{}, false, databaseContextFailure(ctx, "read migration command", err)
	}
	if len(digest) != 32 || len(keyDigest) != 32 || len(job) == 0 || len(receipts) == 0 {
		return commandStored{}, false, ErrResourceLimit
	}
	stored := commandStored{completed: completed, identity: [32]byte(digest)}
	if commandIdentity(job) != stored.identity {
		return commandStored{}, false, ErrCommandConflict
	}
	if err := decodeCommandJSON(job, &stored.job); err != nil {
		return commandStored{}, false, err
	}
	if stored.job.ID != id || stored.job.IdempotencyDigest != [32]byte(keyDigest) {
		return commandStored{}, false, ErrCommandConflict
	}
	if err := decodeCommandJSON(receipts, &stored.receipts); err != nil {
		return commandStored{}, false, err
	}
	if _, err := stored.progress(); err != nil {
		return commandStored{}, false, err
	}
	return stored, true, nil
}

func (s *CommandSession) execute(ctx context.Context, input CommandRequest) (CommandProgress, error) {
	if ctx == nil {
		return CommandProgress{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return CommandProgress{}, err
	}
	req, wire, job, digest, err := canonicalCommand(input)
	if err != nil {
		return CommandProgress{}, err
	}
	bounded, finish, err := s.begin(ctx)
	if err != nil {
		return CommandProgress{}, err
	}
	defer finish()
	stored, exists, err := readNativeCommand(bounded, s.owned.connection, req.ID)
	if err != nil {
		return CommandProgress{}, err
	}
	if tableExists, err := commandRelation(bounded, s.owned.connection); err != nil {
		return CommandProgress{}, err
	} else if tableExists {
		var keyOwner string
		err := s.owned.connection.QueryRowContext(bounded, `SELECT command_id FROM public.golib_migration_commands WHERE idempotency_digest=$1`, req.IdempotencyDigest[:]).Scan(&keyOwner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return CommandProgress{}, databaseContextFailure(bounded, "read migration command key", err)
		}
		if err == nil && keyOwner != req.ID {
			return CommandProgress{}, ErrCommandConflict
		}
	}
	if exists && stored.identity != digest {
		return CommandProgress{}, ErrCommandConflict
	}
	if !exists {
		stored = commandStored{job: wire, identity: digest}
	}
	progress, err := stored.progress()
	if err != nil {
		return CommandProgress{}, err
	}
	// All namespaces are rediscovered before ledger/command preparation. A
	// matching clean suffix is insufficient: only atomic native receipts count.
	for _, h := range req.Histories {
		current, err := readOwnedLedger(bounded, s.owned.connection, h.Namespace, s.taint)
		if err != nil {
			return CommandProgress{}, err
		}
		if _, err := migrations.PlanUp(h.Source, current); err != nil {
			return CommandProgress{}, err
		}
		expected := slices.Clone(h.Base)
		for _, receipt := range progress.receipts {
			if receipt.Namespace == h.Namespace {
				expected = append(expected, receipt.Record)
			}
		}
		if len(expected) != len(current) {
			return CommandProgress{}, ErrCommandConflict
		}
		for i, r := range current {
			if recordWire(r) != recordWire(expected[i]) {
				return CommandProgress{}, ErrCommandConflict
			}
		}
	}
	// Compile before any durable bookkeeping, preserving native syntax/error
	// ownership and preventing a late source defect from authorizing early work.
	adapters := make([][]*gooseadapter.Adapter, len(req.Batches))
	for i, b := range req.Batches {
		for _, m := range b.Migrations {
			a, err := gooseadapter.Compile(m)
			if err != nil {
				return CommandProgress{}, err
			}
			adapters[i] = append(adapters[i], a)
		}
	}
	if !exists {
		if err := s.initializeCommand(bounded, stored, job); err != nil {
			return CommandProgress{}, err
		}
	}
	for i := stored.completed; i < len(req.Batches); i++ {
		if err := bounded.Err(); err != nil {
			return progress, err
		}
		next, err := s.applyCommandBatch(bounded, stored, req.Batches[i], adapters[i])
		if err != nil {
			return progress, err
		}
		stored = next
		progress, err = stored.progress()
		if err != nil {
			return CommandProgress{}, err
		}
	}
	return progress, nil
}

func (s *CommandSession) initializeCommand(ctx context.Context, stored commandStored, job []byte) (err error) {
	exists, err := commandRelation(ctx, s.owned.connection)
	if err != nil {
		return err
	}
	tx, err := s.owned.connection.BeginTx(ctx, nil)
	if err != nil {
		return databaseContextFailure(ctx, "begin migration command", err)
	}
	defer s.rollbackCommand(tx, &err)
	if err := setLocalStatementTimeout(ctx, tx, s.owned.statementTimeout); err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, createCommandTableSQL); err != nil {
			return databaseContextFailure(ctx, "prepare migration commands", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO public.golib_migration_commands(command_id,request_digest,idempotency_digest,job,receipts,completed) VALUES($1,$2,$3,$4,$5,0)`, stored.job.ID, stored.identity[:], stored.job.IdempotencyDigest[:], job, []byte("null")); err != nil {
		return databaseContextFailure(ctx, "reserve migration command", err)
	}
	if err := tx.Commit(); err != nil {
		s.owned.tainted = true
		return errors.Join(ErrCommandOutcomeUnknown, databaseContextFailure(ctx, "commit migration command", err))
	}
	return nil
}

func (s *CommandSession) applyCommandBatch(ctx context.Context, stored commandStored, b CommandBatch, adapters []*gooseadapter.Adapter) (resultState commandStored, err error) {
	tx, err := s.owned.connection.BeginTx(ctx, nil)
	if err != nil {
		return commandStored{}, databaseContextFailure(ctx, "begin migration command batch", err)
	}
	defer s.rollbackCommand(tx, &err)
	if err := setLocalStatementTimeout(ctx, tx, s.owned.statementTimeout); err != nil {
		return commandStored{}, err
	}
	if _, err := tx.ExecContext(ctx, ledgerSQL(createLedgerSQL, b.Namespace)); err != nil {
		return commandStored{}, databaseContextFailure(ctx, "prepare command ledger", err)
	}
	if b.Preflight != "" {
		if _, err := tx.ExecContext(ctx, b.Preflight); err != nil {
			return commandStored{}, databaseContextFailure(ctx, "execute command preflight", err)
		}
	}
	// Clone progress before appending so a failed transaction never mutates the
	// last acknowledged snapshot returned to the caller.
	next := stored
	next.receipts = slices.Clone(stored.receipts)
	for i, m := range b.Migrations {
		started := time.Now().UTC()
		if err := insertDirty(ctx, tx, m, started, b.Namespace); err != nil {
			return commandStored{}, err
		}
		if err := adapters[i].ApplyTx(ctx, tx); err != nil {
			return commandStored{}, err
		}
		finished := time.Now().UTC()
		if err := markClean(ctx, tx, m, finished, finished.Sub(started), b.Namespace); err != nil {
			return commandStored{}, err
		}
	}
	// Read the actual persisted precision rather than recording a higher-
	// precision timestamp/duration that PostgreSQL cannot later reproduce.
	rows, err := tx.QueryContext(ctx, ledgerSQL(ledgerRecordsSQL, b.Namespace))
	if err != nil {
		return commandStored{}, databaseContextFailure(ctx, "read command checkpoint ledger", err)
	}
	records, readErr := decodeLedgerRows(ctx, rows)
	closeErr := rows.Close()
	if readErr != nil || closeErr != nil {
		return commandStored{}, errors.Join(readErr, databaseFailure("close command ledger", closeErr))
	}
	for _, m := range b.Migrations {
		found := false
		for _, r := range records {
			if r.Version() == m.Version() && r.Name() == m.Name() && r.Checksum() == m.Checksum() && !r.Dirty() {
				next.receipts = append(next.receipts, commandReceiptWire{b.Namespace, recordWire(r)})
				found = true
				break
			}
		}
		if !found {
			return commandStored{}, ErrCommandConflict
		}
	}
	next.completed++
	receipts, err := json.Marshal(next.receipts)
	if err != nil || len(receipts) > maxCommandBytes {
		return commandStored{}, ErrResourceLimit
	}
	result, err := tx.ExecContext(ctx, `UPDATE public.golib_migration_commands SET receipts=$1,completed=$2 WHERE command_id=$3 AND request_digest=$4 AND completed=$5`, receipts, next.completed, stored.job.ID, stored.identity[:], stored.completed)
	if err != nil {
		return commandStored{}, databaseContextFailure(ctx, "checkpoint migration command", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return commandStored{}, databaseFailure("read command checkpoint count", err)
	}
	if count != 1 {
		return commandStored{}, ErrCommandConflict
	}
	if err := tx.Commit(); err != nil {
		s.owned.tainted = true
		return commandStored{}, errors.Join(ErrCommandOutcomeUnknown, databaseContextFailure(ctx, "commit command batch", err))
	}
	return next, nil
}

func (s *CommandSession) rollbackCommand(tx *sql.Tx, result *error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		s.taint()
		*result = errors.Join(*result, ErrCommandOutcomeUnknown, databaseFailure("rollback migration command", err))
	}
}
